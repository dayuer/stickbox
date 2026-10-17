package auth

import (
	"sync"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/store"
)

type fakeKeys struct {
	mu    sync.Mutex
	keys  map[string]store.Key
	reads int
}

const testPepper = "test-pepper-0123456789abcdef"

func (f *fakeKeys) KeyByID(id string) (store.Key, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	k, ok := f.keys[id]
	return k, ok, nil
}

func (f *fakeKeys) UpdateKeyHash(id, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.keys[id]
	k.Hash = hash
	f.keys[id] = k
	return nil
}

func newFixture(t *testing.T) (*Verifier, *fakeKeys, string) {
	t.Helper()
	keyID, secret, token := NewToken()
	h, err := HashSecret(testPepper, secret)
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

// argon2 单次几十毫秒，取件路径每次都算会毁掉低延迟目标。
func TestVerifyUsesCacheOnSecondCall(t *testing.T) {
	v, f, token := newFixture(t)
	if _, err := v.Verify(token); err != nil {
		t.Fatal(err)
	}
	before := f.reads
	start := time.Now()
	if _, err := v.Verify(token); err != nil {
		t.Fatal(err)
	}
	if f.reads != before {
		t.Errorf("第二次校验又查了库（reads %d → %d），缓存没生效", before, f.reads)
	}
	if d := time.Since(start); d > 5*time.Millisecond {
		t.Errorf("缓存命中耗时 %v，说明仍在跑 argon2", d)
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

// 配对转正后必须让缓存失效，否则旧缓存里的 Pending=true 还会挡一小时。
func TestPendingCacheInvalidatedAfterActivation(t *testing.T) {
	v, f, token := newFixture(t)
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
	v.Invalidate(keyID)

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

func (f *blockingKeys) UpdateKeyHash(string, string) error { return nil }

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
	h, err := HashSecret(testPepper, secret)
	if err != nil {
		t.Fatal(err)
	}
	f := &blockingKeys{
		key: store.Key{KeyID: keyID, Dev: "dev1", Role: string(RoleDevice),
			Hash: h, Pending: true},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	v := NewVerifier(f, testPepper)

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
	if got.id.Pending || got.id.UserID != "u1" {
		t.Fatalf("正在进行的校验返回了旧身份 %+v，期望已转正", got.id)
	}
	if f.reads < 2 {
		t.Errorf("转正后应重新查库，reads=%d", f.reads)
	}
	if id, err := v.Verify(token); err != nil || id.Pending {
		t.Fatalf("后续校验 = %+v, %v；缓存里残留了 pending", id, err)
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
	h, err := HashSecret(testPepper, secret)
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
