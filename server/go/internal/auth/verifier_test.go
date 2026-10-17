package auth

import (
	"errors"
	"sync"
	"testing"

	"github.com/dayuer/stickbox/server/go/internal/store"
)

type fakeKeys struct {
	mu    sync.Mutex
	keys  map[string]store.Key
	reads int
	err   error
}

const testPepper = "test-pepper-0123456789abcdef"

func (f *fakeKeys) KeyByID(id string) (store.Key, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return store.Key{}, false, f.err
	}
	k, ok := f.keys[id]
	return k, ok, nil
}

func (f *fakeKeys) UpdateKeyHash(id, oldHash, newHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.keys[id]
	if k.Hash == oldHash {
		k.Hash = newHash
		f.keys[id] = k
	}
	return nil
}

func newFixture(t *testing.T) (*Verifier, *fakeKeys, string) {
	t.Helper()
	keyID, secret, token := NewToken()
	h, err := HashSecret(testPepper, keyID, secret)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeKeys{keys: map[string]store.Key{
		keyID: {KeyID: keyID, Dev: "dev1", Role: string(RoleDevice), Hash: h},
	}}
	return NewVerifier(f, testPepper), f, token
}

func TestVerifyReturnsIdentity(t *testing.T) {
	v, _, token := newFixture(t)
	id, err := v.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if id.Dev != "dev1" || id.Role != RoleDevice {
		t.Errorf("身份 = %+v", id)
	}
}

// 压测对照路径：命中缓存也必须查一次库，否则 CLI 在别的进程吊销后
// 本进程会继续放行（这正是 T25 要修的线上问题）。
func TestCachedVerifyRechecksDBOnHit(t *testing.T) {
	_, f, token := newFixture(t)
	v := newVerifier(f, testPepper, true)
	if _, err := v.Verify(token); err != nil {
		t.Fatal(err)
	}
	before := f.reads
	if _, err := v.Verify(token); err != nil {
		t.Fatal(err)
	}
	if f.reads != before+1 {
		t.Errorf("缓存命中后查库次数 = %d，期望 %d（命中也要核对吊销状态）", f.reads, before+1)
	}
}

// 生产路径不缓存正结果：吊销后不需要 Invalidate，下一次校验立即拒绝。
func TestRevokedKeyDeniedWithoutInvalidate(t *testing.T) {
	v, f, token := newFixture(t)
	if _, err := v.Verify(token); err != nil {
		t.Fatal(err)
	}
	keyID, _, _ := SplitToken(token)
	k := f.keys[keyID]
	k.Disabled = true
	f.keys[keyID] = k
	if _, err := v.Verify(token); !errors.Is(err, ErrDenied) {
		t.Fatalf("吊销后 Verify = %v，期望 ErrDenied", err)
	}
}

// DB 错误不能伪装成“密钥无效”，broker 要靠这个区分 0x03 和 0x05。
func TestVerifyDBErrorIsNotDenied(t *testing.T) {
	v, f, token := newFixture(t)
	f.err = errors.New("db unavailable")
	if _, err := v.Verify(token); err == nil || errors.Is(err, ErrDenied) {
		t.Fatalf("DB 错误返回 %v，不能是 ErrDenied", err)
	}
}

// pending 必须出现在 Identity 里，所有数据权限判定都靠它。
func TestVerifyReportsPending(t *testing.T) {
	v, f, token := newFixture(t)
	keyID, _, _ := SplitToken(token)
	k := f.keys[keyID]
	k.Pending = true
	f.keys[keyID] = k

	id, err := v.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Pending {
		t.Errorf("pending 密钥的身份 = %+v", id)
	}
}

// 配对转正后即使没有 Invalidate，缓存命中重查也能看到新状态。
func TestCachedVerifySeesActivationWithoutInvalidate(t *testing.T) {
	_, f, token := newFixture(t)
	v := newVerifier(f, testPepper, true)
	keyID, _, _ := SplitToken(token)
	k := f.keys[keyID]
	k.Pending = true
	f.keys[keyID] = k

	if id, _ := v.Verify(token); !id.Pending {
		t.Fatal("第一次校验不是 pending")
	}

	k.Pending = false
	k.UserID = "u1"
	f.keys[keyID] = k

	id, err := v.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if id.Pending || id.UserID != "u1" {
		t.Errorf("转正后的身份 = %+v", id)
	}
}

func TestVerifyRejects(t *testing.T) {
	v, f, token := newFixture(t)
	keyID, _, _ := SplitToken(token)

	if _, err := v.Verify(keyID + ".wrongsecret"); err == nil {
		t.Error("错误密钥应被拒")
	}
	if _, err := v.Verify("nonexistent.secret"); err == nil {
		t.Error("不存在的 key_id 应被拒")
	}
	if _, err := v.Verify("malformed"); err == nil {
		t.Error("畸形令牌应被拒")
	}

	k := f.keys[keyID]
	k.Disabled = true
	f.keys[keyID] = k
	v.Invalidate(keyID)
	if _, err := v.Verify(token); err == nil {
		t.Error("已吊销的密钥应被拒")
	}
}

func TestInvalidateDropsCache(t *testing.T) {
	v, f, token := newFixture(t)
	v.Verify(token)
	keyID, _, _ := SplitToken(token)
	v.Invalidate(keyID)
	before := f.reads
	v.Verify(token)
	if f.reads == before {
		t.Error("Invalidate 后应重新查库")
	}
}

// 重连风暴：几十个并发校验不能把内存打爆，也不能出错。
// 信号量把同时进行的 argon2 限制在 8 个以内。
// blockingKeys 让第一次 KeyByID 卡在半路，模拟「Verify 已读到旧状态、
// 正在跑 argon2 时配对转正」的竞态。
type blockingKeys struct {
	mu      sync.Mutex
	key     store.Key
	reads   int
	entered chan struct{}
	release chan struct{}
}

func (f *blockingKeys) UpdateKeyHash(string, string, string) error { return nil }

func (f *blockingKeys) KeyByID(string) (store.Key, bool, error) {
	f.mu.Lock()
	k := f.key
	f.reads++
	first := f.reads == 1
	f.mu.Unlock()
	if first {
		close(f.entered)
		<-f.release
	}
	return k, true, nil
}

// 设备连接与配对同时发生时，Verify 可能读到 pending 并正在算 argon2；
// 此时配对转正并 Invalidate。旧实现会在 Invalidate 之后把 pending 身份
// 写回缓存，之后一小时的重连全部被当成待配对。修复后必须重新查库。
func TestVerifyDoesNotCacheIdentityInvalidatedMidFlight(t *testing.T) {
	keyID, secret, token := NewToken()
	h, err := HashSecret(testPepper, keyID, secret)
	if err != nil {
		t.Fatal(err)
	}
	f := &blockingKeys{
		key: store.Key{KeyID: keyID, Dev: "dev1", Role: string(RoleDevice),
			Hash: h, Pending: true},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	v := newVerifier(f, testPepper, true)

	type result struct {
		id  Identity
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, err := v.Verify(token)
		done <- result{id, err}
	}()
	<-f.entered // Verify 已经读到 pending

	// 配对转正 + 缓存失效，然后放行这次校验。
	f.mu.Lock()
	k := f.key
	k.Pending = false
	k.UserID = "u1"
	f.key = k
	f.mu.Unlock()
	v.Invalidate(keyID)
	close(f.release)

	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	_ = got // 飞行中的那次可能返回校验开始时的旧状态；关键是下一次

	before := f.reads
	if id, err := v.Verify(token); err != nil || id.Pending {
		t.Fatalf("后续校验 = %+v, %v；缓存命中没有重查库", id, err)
	}
	if f.reads <= before {
		t.Errorf("后续校验应重新查库，reads=%d", f.reads)
	}
}

// 旧 argon2 记录校验成功后应就地迁移成 HMAC，且迁移后仍能登录。
func TestVerifyMigratesLegacyArgon2(t *testing.T) {
	keyID, secret, token := NewToken()
	legacy, err := hashArgon2(secret)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeKeys{keys: map[string]store.Key{
		keyID: {KeyID: keyID, Dev: "dev1", Role: string(RoleDevice), Hash: legacy},
	}}
	v := NewVerifier(f, testPepper)
	if _, err := v.Verify(token); err != nil {
		t.Fatalf("旧记录校验失败：%v", err)
	}
	if got := f.keys[keyID].Hash; got == legacy || got[:len("$hmac-sha256$")] != "$hmac-sha256$" {
		t.Fatalf("旧记录没有迁移成 HMAC：%q", got)
	}
	v.Invalidate(keyID)
	if _, err := v.Verify(token); err != nil {
		t.Fatalf("迁移后的 HMAC 校验失败：%v", err)
	}
}

func TestVerifyRejectsWrongPepper(t *testing.T) {
	keyID, secret, token := NewToken()
	h, err := HashSecret(testPepper, keyID, secret)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeKeys{keys: map[string]store.Key{
		keyID: {KeyID: keyID, Role: string(RoleDevice), Hash: h},
	}}
	v := NewVerifier(f, "another-pepper-0123456789")
	if _, err := v.Verify(token); err == nil {
		t.Fatal("换 pepper 后旧哈希不应通过")
	}
}

func TestVerifyConcurrentIsBounded(t *testing.T) {
	v, _, token := newFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Verify(token); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发校验出错：%v", err)
	}
}
