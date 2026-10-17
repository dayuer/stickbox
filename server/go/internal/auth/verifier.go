package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"regexp"
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
)

type Identity struct {
	Dev    string // device 角色专用
	UserID string // agent 角色专用；不要用它判定设备的数据权限
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
}

const cacheTTL = time.Hour

// argon2 每次校验占 64MB 内存。一万台设备同时重连（服务重启后就是这个场景）
// 时并发跑会瞬间吃掉几十 GB，所以同时进行的校验数限制为 8，超出的排队等待。
// 这条比缓存更要命——缓存冷的时候正是重连风暴。
const argonConcurrency = 8

type cacheEntry struct {
	sum [32]byte // sha256(secret)，缓存命中时只做常数时间比较
	id  Identity
	exp time.Time
}

type Verifier struct {
	keys KeyStore
	mu   sync.RWMutex
	c    map[string]cacheEntry
	// epoch 在每次 Invalidate/InvalidateAll 时加一。Verify 在跑完 argon2
	// 准备写缓存前检查它：如果期间有失效操作，说明刚查到的身份可能已经
	// 过时（例如设备连接与配对同时发生），必须重新查库而不是写回缓存。
	epoch uint64
	sem   chan struct{}
	now   func() time.Time
}

func NewVerifier(keys KeyStore) *Verifier {
	return &Verifier{
		keys: keys,
		c:    map[string]cacheEntry{},
		sem:  make(chan struct{}, argonConcurrency),
		now:  time.Now,
	}
}

// Verify 校验 "{key_id}.{secret}" 形式的令牌。
//
// 首次走 argon2id（几十毫秒，故意的）；之后缓存 sha256，
// 让取件这种高频路径退化成一次哈希比较。
func (v *Verifier) Verify(token string) (Identity, error) {
	keyID, secret, err := SplitToken(token)
	if err != nil {
		return Identity{}, ErrDenied
	}
	sum := sha256.Sum256([]byte(secret))

	// 最多重试 3 次：Invalidate 一般不会连续发生，这里只是防止异常情况下
	// 无限循环。最后一次按刚读到的库返回，但不写缓存。
	for attempt := 0; ; attempt++ {
		v.mu.RLock()
		e, ok := v.c[keyID]
		epoch := v.epoch
		v.mu.RUnlock()
		if ok && v.now().Before(e.exp) {
			if subtle.ConstantTimeCompare(sum[:], e.sum[:]) == 1 {
				return e.id, nil
			}
			return Identity{}, ErrDenied
		}

		k, found, err := v.keys.KeyByID(keyID)
		if err != nil {
			return Identity{}, err
		}
		if !found || k.Disabled {
			return Identity{}, ErrDenied
		}

		v.sem <- struct{}{} // 限制并发 argon2，防重连风暴打爆内存
		okPass := VerifySecret(secret, k.Hash)
		<-v.sem
		if !okPass {
			return Identity{}, ErrDenied
		}

		id := Identity{
			Dev: k.Dev, UserID: k.UserID, Role: Role(k.Role), KeyID: k.KeyID,
			Pending: k.Pending,
		}
		v.mu.Lock()
		if v.epoch == epoch {
			v.c[keyID] = cacheEntry{sum: sum, id: id, exp: v.now().Add(cacheTTL)}
			v.mu.Unlock()
			return id, nil
		}
		v.mu.Unlock()
		if attempt >= 2 {
			return id, nil
		}
		// 校验期间缓存被失效（配对转正 / 解绑吊销）：刚读到的身份可能
		// 已经过时，重新查库，绝不能把旧状态写回缓存。
	}
}

// Invalidate 在吊销密钥后立即调用，否则缓存会让它再活最多一小时。
func (v *Verifier) Invalidate(keyID string) {
	v.mu.Lock()
	delete(v.c, keyID)
	v.epoch++
	v.mu.Unlock()
}

// InvalidateAll 清空整张缓存。注销账号时用——那一刻有多把 token 同时失效，
// 逐个 Invalidate 要先把 key_id 全查出来，不如整表清掉重建。
// 代价是在线连接下一次校验要重跑 argon2；注销是极低频操作，可以接受。
func (v *Verifier) InvalidateAll() {
	v.mu.Lock()
	v.c = map[string]cacheEntry{}
	v.epoch++
	v.mu.Unlock()
}
