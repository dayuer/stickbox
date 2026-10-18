package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/store"
)

type Role string

const (
	RoleDevice Role = "device"
	// RoleAgent 是用户电脑上的 stickbox-agent：上报用量、管理名下的屏。
	// 它不能连 MQTT。
	RoleAgent Role = "agent"
	// RoleUser 预留给直接代表用户的令牌（例如将来的 App/网页端）。
	// 与 agent 一样只能走 HTTP，不能连 MQTT。
	RoleUser Role = "user"
)

type Identity struct {
	Dev    string // device 角色专用
	UserID string // agent / user 角色专用；不要用它判定设备的数据权限
	Role   Role
	KeyID  string
	// Pending 表示这把 device 密钥还没完成配对。待配对密钥可以连 MQTT
	// 收配对通知，但绝对不能读 usage / frame 这类用户数据。
	// 权限判定要看这把密钥自己的记录（按 KeyID 查库），不要按 dev 反查主人。
	Pending bool
}

var ErrDenied = errors.New("auth: 拒绝")

// devRe 是设备 ID 的形状：STA MAC 去掉冒号的小写十六进制，12 个字符
// （docs/API.md 第 0 节）。
var devRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ValidDev 判断这个字符串是不是合法的设备 ID。
func ValidDev(s string) bool { return devRe.MatchString(s) }

// KeyStore 是 auth 对 store 的窄依赖，测试里用假实现替换。
type KeyStore interface {
	KeyByID(keyID string) (store.Key, bool, error)
	// UpdateKeyHash 用于把旧 argon2id 记录就地迁移成 HMAC。
	// oldHash 是本次校验时读到的值，作为 CAS 条件。
	UpdateKeyHash(keyID, oldHash, newHash string) error
}

const cacheTTL = time.Hour

// 旧 argon2 记录每次校验占 64MB。迁移后新记录走 HMAC，不再需要排队；
// 这里保留信号量只为迁移期兜底（缓存冷 + 大量旧记录同时重连）。
const argonConcurrency = 8

type cacheEntry struct {
	sum [32]byte // sha256(secret)，缓存命中时只做常数时间比较
	id  Identity
	exp time.Time
}

type Verifier struct {
	keys   KeyStore
	pepper string
	mu     sync.RWMutex
	c      map[string]cacheEntry
	// cache 只用于压测对照（见 verifier_bench_test.go）：生产不缓存正结果。
	// CLI 的 device unbind/revoke 在另一个进程改库，没法让本进程的缓存失效；
	// 每次 CONNECT 都查一次主键，才能在下一次连接就回 0x05。
	cache bool
	sem   chan struct{}
	now   func() time.Time
}

func NewVerifier(keys KeyStore, pepper string) *Verifier {
	return newVerifier(keys, pepper, false)
}

// NewVerifierCached 仅供压测对照：命中缓存后仍会查库核对吊销状态，
// 语义与生产一致，但多一次缓存查找。生产路径不要用它。
func NewVerifierCached(keys KeyStore, pepper string) *Verifier {
	return newVerifier(keys, pepper, true)
}

// newVerifier 只给包内压测用：cache=true 时复现旧的缓存策略（命中仍查库）。
func newVerifier(keys KeyStore, pepper string, cache bool) *Verifier {
	return &Verifier{
		keys:   keys,
		pepper: pepper,
		c:      map[string]cacheEntry{},
		cache:  cache,
		sem:    make(chan struct{}, argonConcurrency),
		now:    time.Now,
	}
}

// HashSecret 用本进程的 pepper 生成存储哈希（签发路径用）。
func (v *Verifier) HashSecret(keyID, secret string) (string, error) {
	return HashSecret(v.pepper, keyID, secret)
}

// Verify 校验 "{key_id}.{secret}" 形式的令牌。
//
// 每次都查库（主键），拿到 disabled/pending/dev 的最新状态；T22 之后密钥
// 哈希是 HMAC，校验本身很便宜。不在进程内缓存正结果是刻意的：CLI 的
// device unbind/revoke 跑在另一个进程，无法让这里的缓存失效，缓存 1 小时
// 会让被吊销的密钥继续 CONNECT 成功。
func (v *Verifier) Verify(token string) (Identity, error) {
	keyID, secret, err := SplitToken(token)
	if err != nil {
		return Identity{}, ErrDenied
	}
	sum := sha256.Sum256([]byte(secret))

	// 仅压测对照：缓存命中也要查库确认状态，不能只比对 secret。
	if v.cache {
		if id, hit, err := v.cachedHit(keyID, sum); hit {
			return id, err
		}
	}

	k, found, err := v.keys.KeyByID(keyID)
	if err != nil {
		return Identity{}, err
	}
	if !found || k.Disabled {
		return Identity{}, ErrDenied
	}

	// 新记录是 HMAC，几乎零成本；只有旧 argon2id 记录才需要限并发。
	// 先做一次格式判断，避免付 64MB 的钱去跑一个 HMAC 比较。
	okPass, legacy := false, false
	if strings.HasPrefix(k.Hash, "$"+hmacScheme+"$") {
		okPass, _ = VerifySecret(v.pepper, keyID, secret, k.Hash)
	} else {
		v.sem <- struct{}{}
		okPass, legacy = VerifySecret(v.pepper, keyID, secret, k.Hash)
		<-v.sem
	}
	if !okPass {
		return Identity{}, ErrDenied
	}
	if legacy {
		// 校验成功即迁移；失败不影响本次登录（下次再试）。
		// 带上旧哈希做 CAS：并发校验/轮换时不会把别人刚写的新哈希盖回去。
		if h, err := v.HashSecret(keyID, secret); err == nil {
			if err := v.keys.UpdateKeyHash(keyID, k.Hash, h); err != nil {
				slog.Warn("令牌哈希迁移失败", "key_id", keyID, "err", err)
			}
		}
	}

	id := identityOf(k)
	if v.cache {
		v.mu.Lock()
		v.c[keyID] = cacheEntry{sum: sum, id: id, exp: v.now().Add(cacheTTL)}
		v.mu.Unlock()
	}
	return id, nil
}

func identityOf(k store.Key) Identity {
	return Identity{
		Dev: k.Dev, UserID: k.UserID, Role: Role(k.Role), KeyID: k.KeyID,
		Pending: k.Pending,
	}
}

// cachedHit 只在压测对照路径使用：命中缓存后仍然查一次库，确认密钥
// 没有被 CLI 吊销、状态没有变化。DB 错误原样返回（由 broker 回 0x03）。
func (v *Verifier) cachedHit(keyID string, sum [32]byte) (Identity, bool, error) {
	v.mu.RLock()
	e, ok := v.c[keyID]
	v.mu.RUnlock()
	if !ok || !v.now().Before(e.exp) {
		return Identity{}, false, nil
	}
	if subtle.ConstantTimeCompare(sum[:], e.sum[:]) != 1 {
		return Identity{}, true, ErrDenied
	}
	k, found, err := v.keys.KeyByID(keyID)
	if err != nil {
		return Identity{}, true, err
	}
	if !found || k.Disabled {
		return Identity{}, true, ErrDenied
	}
	id := identityOf(k)
	v.mu.Lock()
	v.c[keyID] = cacheEntry{sum: sum, id: id, exp: v.now().Add(cacheTTL)}
	v.mu.Unlock()
	return id, true, nil
}

// Lookup 实时查库判断一把密钥现在是否仍然有效，绕过缓存。
//
// 已在线连接的 ACL 用的是连接建立时的缓存身份；CLI 的 device unbind /
// device revoke 直接改库、不会踢线，所以下一次认证或心跳必须用这里
// 重新核对，才能发现密钥已经被吊销/禁用。
// 返回 (零值, false, nil) 表示密钥不存在或已吊销；DB 错误单独返回，
// 调用方不应把一次查询失败当成吊销。
func (v *Verifier) Lookup(keyID string) (Identity, bool, error) {
	k, ok, err := v.keys.KeyByID(keyID)
	if err != nil {
		return Identity{}, false, err
	}
	if !ok || k.Disabled {
		return Identity{}, false, nil
	}
	return Identity{
		Dev: k.Dev, UserID: k.UserID, Role: Role(k.Role), KeyID: k.KeyID,
		Pending: k.Pending,
	}, true, nil
}

// Invalidate 在吊销密钥后立即调用，否则缓存会让它再活最多一小时。
func (v *Verifier) Invalidate(keyID string) {
	v.mu.Lock()
	delete(v.c, keyID)
	v.mu.Unlock()
}

// InvalidateAll 清空整张缓存。注销账号时用——那一刻有多把 token 同时失效，
// 逐个 Invalidate 要先把 key_id 全查出来，不如整表清掉重建。
// 代价是在线连接下一次校验要重跑 argon2；注销是极低频操作，可以接受。
func (v *Verifier) InvalidateAll() {
	v.mu.Lock()
	v.c = map[string]cacheEntry{}
	v.mu.Unlock()
}
