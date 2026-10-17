package frames

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/frame"
	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/store"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

// —— 假的依赖 ——

type fakeStore struct {
	devs   map[string][]string
	owners map[string]string
	keys   map[string]store.Key
	err    error
}

func (f *fakeStore) DevicesOfUser(userID string) ([]string, error) {
	return f.devs[userID], f.err
}

func (f *fakeStore) OwnerOfDevice(dev string) (string, bool, error) {
	u, ok := f.owners[dev]
	return u, ok, f.err
}

func (f *fakeStore) KeyByID(keyID string) (store.Key, bool, error) {
	k, ok := f.keys[keyID]
	return k, ok, f.err
}

func (f *fakeStore) ListUsers() ([]store.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]store.User, 0, len(f.devs))
	for id := range f.devs {
		out = append(out, store.User{ID: id})
	}
	return out, nil
}

type fakeUsage struct {
	mu      sync.Mutex
	reports map[string]usage.Report
	at      map[string]time.Time
	ok      bool
	err     error
}

func (f *fakeUsage) Latest(userID string) (usage.Report, time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil || !f.ok {
		return usage.Report{}, time.Time{}, false, f.err
	}
	r, has := f.reports[userID]
	if !has {
		return usage.Report{}, time.Time{}, false, nil
	}
	return r, f.at[userID], true, nil
}

func (f *fakeUsage) set(userID string, r usage.Report, at time.Time) {
	f.mu.Lock()
	f.reports[userID] = r
	f.at[userID] = at
	f.mu.Unlock()
}

type fakePub struct {
	mu   sync.Mutex
	sent map[string][][]byte
	err  error
}

func (f *fakePub) PublishFrame(dev string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.sent == nil {
		f.sent = map[string][][]byte{}
	}
	f.sent[dev] = append(f.sent[dev], append([]byte(nil), payload...))
	return nil
}

func (f *fakePub) last(dev string) (ver uint64, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := f.sent[dev]
	if len(msgs) == 0 {
		return 0, false
	}
	var p struct {
		Ver uint64 `json:"ver"`
	}
	if json.Unmarshal(msgs[len(msgs)-1], &p) != nil {
		return 0, false
	}
	return p.Ver, true
}

func (f *fakePub) count(dev string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent[dev])
}

type fakeBuilder struct {
	mu           sync.Mutex
	calls        int
	failErr      error
	payload      string
	extra        int
	lastSnapshot source.Snapshot
}

func (f *fakeBuilder) Build(s source.Snapshot) (*frame.Bundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastSnapshot = s
	if f.failErr != nil {
		return nil, f.failErr
	}
	p := f.payload
	if p == "" && len(s.Windows) > 0 {
		p = fmt.Sprintf("%.0f", s.Windows[0].UsedPercentage*100)
	}
	return testBundleN(p, f.extra), nil
}

func (f *fakeBuilder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeBuilder) snapshot() source.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSnapshot
}

func (f *fakeBuilder) setFail(err error) {
	f.mu.Lock()
	f.failErr = err
	f.mu.Unlock()
}

func (f *fakeBuilder) setExtra(n int) {
	f.mu.Lock()
	f.extra = n
	f.mu.Unlock()
}

func testBundleN(payload string, extra int) *frame.Bundle {
	base := append([]byte("base:"+payload), make([]byte, extra)...)
	atlas := []byte("atlas:" + payload)
	b := &frame.Bundle{Base: base, Atlas: atlas}
	b.Meta.V = frame.Version
	bs := sha256.Sum256(base)
	as := sha256.Sum256(atlas)
	b.Meta.Frame = hex.EncodeToString(bs[:])
	b.Meta.Atlas.SHA256 = hex.EncodeToString(as[:])
	b.Meta.Atlas.Size = len(atlas)
	return b
}

func testBundle(payload string) *frame.Bundle { return testBundleN(payload, 0) }

type fixture struct {
	t    *testing.T
	root string
	svc  *Service
	st   *fakeStore
	us   *fakeUsage
	pub  *fakePub
	b    *fakeBuilder

	mu  sync.Mutex
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t:    t,
		root: t.TempDir(),
		st: &fakeStore{
			devs:   map[string][]string{"u1": {"d1", "d2"}, "u2": {"d3"}, "u3": {"d4"}},
			owners: map[string]string{"d1": "u1", "d2": "u1", "d3": "u2", "d4": "u3"},
			keys: map[string]store.Key{
				"k1": {KeyID: "k1", Dev: "d1", Role: "device", UserID: "u1"},
				"k3": {KeyID: "k3", Dev: "d3", Role: "device", UserID: "u2"},
				"k4": {KeyID: "k4", Dev: "d4", Role: "device", UserID: "u3"},
				"kp": {KeyID: "kp", Dev: "d1", Role: "device", Pending: true},
				"kx": {KeyID: "kx", Dev: "d1", Role: "device", UserID: "u1", Disabled: true},
				"ku": {KeyID: "ku", Dev: "d9", Role: "device", UserID: "u1"},
				"kn": {KeyID: "kn", Dev: "d1", Role: "device"},
			},
		},
		us: &fakeUsage{
			ok: true,
			reports: map[string]usage.Report{
				"u1": {FiveHour: &usage.Window{UsedPercentage: 42, ResetsAt: 1792228800}},
				"u2": {FiveHour: &usage.Window{UsedPercentage: 7, ResetsAt: 1792228800}},
			},
			at: map[string]time.Time{
				"u1": time.Date(2026, 10, 17, 11, 58, 0, 0, time.UTC),
				"u2": time.Date(2026, 10, 17, 11, 58, 0, 0, time.UTC),
			},
		},
		pub: &fakePub{},
		b:   &fakeBuilder{},
		now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
	}
	svc, err := New(f.root, f.st, f.us, f.pub, f.b, Options{
		DefaultTZ:         "UTC",
		Debounce:          5 * time.Millisecond,
		RenderConcurrency: 1,
		Now:               f.currentTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	t.Cleanup(svc.Close)
	return f
}

func (f *fixture) currentTime() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fixture) setNow(t time.Time) {
	f.mu.Lock()
	f.now = t
	f.mu.Unlock()
}

func (f *fixture) rebuild(userID string) {
	f.t.Helper()
	if err := f.svc.Rebuild(userID); err != nil {
		f.t.Fatalf("Rebuild(%s) = %v", userID, err)
	}
}

func (f *fixture) version(userID string) uint64 {
	f.t.Helper()
	_, ver, ok := f.svc.Current(userID)
	if !ok {
		f.t.Fatalf("%s 没有帧", userID)
	}
	return ver
}

func (f *fixture) waitVersion(userID string, want uint64, timeout time.Duration) {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, ver, ok := f.svc.Current(userID); ok && ver >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("等待 %s 版本 %d 超时", userID, want)
}

// —— 测试 ——

func TestRebuildIncrementsVersionAndNotifies(t *testing.T) {
	f := newFixture(t)
	f.rebuild("u1")
	if got := f.version("u1"); got != 1 {
		t.Fatalf("第一次版本 = %d，期望 1", got)
	}
	if ver, ok := f.pub.last("d1"); !ok || ver != 1 {
		t.Errorf("d1 通知 = %d,%v，期望 ver=1", ver, ok)
	}
	if ver, ok := f.pub.last("d2"); !ok || ver != 1 {
		t.Errorf("d2 通知 = %d,%v，期望 ver=1", ver, ok)
	}
	data, _, ok := f.svc.Current("u1")
	if !ok {
		t.Fatal("没有帧")
	}
	b, err := Unpack(data)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if string(b.Base) != "base:42" {
		t.Errorf("底帧 = %q，期望 base:42", b.Base)
	}

	f.rebuild("u1")
	if got := f.version("u1"); got != 2 {
		t.Fatalf("第二次版本 = %d，期望 2", got)
	}
	if ver, _ := f.pub.last("d1"); ver != 2 {
		t.Errorf("d1 第二次通知 = %d，期望 2", ver)
	}
}

func TestBuildFailureKeepsOldFrame(t *testing.T) {
	f := newFixture(t)
	f.rebuild("u1")
	old, oldVer, _ := f.svc.Current("u1")
	oldCopy := append([]byte(nil), old...)

	f.b.setFail(errors.New("band cap exceeded"))
	if err := f.svc.Rebuild("u1"); err == nil {
		t.Fatal("构建失败应返回错误")
	}
	now, ver, ok := f.svc.Current("u1")
	if !ok || ver != oldVer || string(now) != string(oldCopy) {
		t.Errorf("构建失败后帧变了：ver %d→%d", oldVer, ver)
	}
}

func TestPackOverLimitKeepsOldFrame(t *testing.T) {
	f := newFixture(t)
	f.rebuild("u1")
	before := f.version("u1")

	f.b.setExtra(MaxPackBytes) // 底帧本身就超过 256K 槽位
	if err := f.svc.Rebuild("u1"); err == nil {
		t.Fatal("超过 FP_MAX_PACK 应该失败")
	}
	if got := f.version("u1"); got != before {
		t.Errorf("超限后版本 = %d，期望保持 %d", got, before)
	}
}

func TestMarkDirtyDebounces(t *testing.T) {
	f := newFixture(t)
	f.svc.MarkDirty("u1")
	f.svc.MarkDirty("u1")
	f.svc.MarkDirty("u1")
	f.waitVersion("u1", 1, 2*time.Second)
	if got := f.b.count(); got != 1 {
		t.Errorf("防抖后渲染了 %d 次，期望 1", got)
	}
	f.svc.MarkDirty("u1")
	f.waitVersion("u1", 2, 2*time.Second)
	if got := f.b.count(); got != 2 {
		t.Errorf("第二次标脏后渲染了 %d 次，期望 2", got)
	}
}

// 新部署/重启：已配对、有设备但还没有帧缓存的用户要在启动巡检时自动出帧；
// 没有用量的用户也出一张「等待上报」帧。
func TestStartupRendersExistingUsers(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.svc.Run(ctx)

	for _, u := range []string{"u1", "u2", "u3"} {
		f.waitVersion(u, 1, 2*time.Second)
	}
	// 版本写入缓存后才发通知，这里轮询等通知落地。
	waitPub := func(dev string) {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if ver, ok := f.pub.last(dev); ok && ver == 1 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("%s 没有收到 ver=1 的帧通知", dev)
	}
	waitPub("d1")
	waitPub("d4")
}

// 持续每 2 秒上报时，防抖不能无限重置；maxWait 到点必须渲染。
func TestDebounceMaxWait(t *testing.T) {
	f := newFixture(t)
	f.svc.opt.Debounce = 200 * time.Millisecond
	f.svc.opt.MaxWait = 30 * time.Millisecond

	start := time.Now()
	go func() {
		for i := 0; i < 20; i++ {
			f.svc.MarkDirty("u1")
			time.Sleep(5 * time.Millisecond)
		}
	}()
	f.waitVersion("u1", 1, 1*time.Second)
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("maxWait 没生效，渲染等了 %v", elapsed)
	}
}

func TestOlderSnapshotDiscarded(t *testing.T) {
	f := newFixture(t)
	f.rebuild("u1")
	before := f.version("u1")

	// 收到一份比当前帧更旧的用量（并发上报的乱序快照）。
	f.us.set("u1", usage.Report{FiveHour: &usage.Window{UsedPercentage: 1, ResetsAt: 1792228800}},
		time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC))
	if err := f.svc.Rebuild("u1"); err != nil {
		t.Fatal(err)
	}
	if got := f.version("u1"); got != before {
		t.Errorf("旧快照把版本推进到了 %d，期望保持 %d", got, before)
	}
}

func TestRebuildUsesUserTimezone(t *testing.T) {
	f := newFixture(t)
	f.us.set("u1", usage.Report{
		FiveHour: &usage.Window{UsedPercentage: 10, ResetsAt: 1792228800},
		TZ:       "Asia/Bangkok",
	}, time.Date(2026, 10, 17, 19, 0, 0, 0, time.UTC))
	f.setNow(time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC))

	f.rebuild("u1")
	snap := f.b.snapshot()
	if snap.Now.Location().String() != "Asia/Bangkok" {
		t.Fatalf("快照时区 = %s，期望 Asia/Bangkok", snap.Now.Location())
	}
	if snap.Now.Hour() != 3 || snap.Now.Day() != 18 {
		t.Errorf("快照本地时间 = %s，期望 10-18 03:00", snap.Now)
	}
}

func TestCheckStaleAcrossDayBoundary(t *testing.T) {
	f := newFixture(t)
	f.us.set("u1", usage.Report{
		FiveHour: &usage.Window{UsedPercentage: 10, ResetsAt: 1792228800},
		TZ:       "Asia/Bangkok",
	}, time.Date(2026, 10, 17, 23, 0, 0, 0, time.UTC))
	f.setNow(time.Date(2026, 10, 17, 23, 0, 0, 0, time.UTC))
	f.rebuild("u1")

	// 曼谷时间已跨到下一天：2026-10-18 00:30 +07。
	f.setNow(time.Date(2026, 10, 18, 17, 30, 0, 0, time.UTC))
	f.svc.checkStale(f.currentTime())
	f.waitVersion("u1", 2, 2*time.Second)
}

func TestCheckStaleAfter15Minutes(t *testing.T) {
	f := newFixture(t)
	f.rebuild("u1")

	f.setNow(f.currentTime().Add(16 * time.Minute))
	f.svc.checkStale(f.currentTime())
	f.waitVersion("u1", 2, 2*time.Second)

	// 刚重渲过，再检查不应该继续每 tick 都重渲 u1（其他用户首次补帧不算）。
	f.svc.checkStale(f.currentTime())
	time.Sleep(50 * time.Millisecond)
	if _, ver, _ := f.svc.Current("u1"); ver != 2 {
		t.Errorf("u1 在同一时刻被重复重渲，版本 = %d", ver)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	f := newFixture(t)
	f.rebuild("u1")
	want, ver, _ := f.svc.Current("u1")
	f.svc.Close() // 停掉 worker

	b2 := &fakeBuilder{failErr: errors.New("不该重新渲染")}
	svc2, err := New(f.root, f.st, f.us, f.pub, b2, Options{
		DefaultTZ: "UTC", Debounce: 5 * time.Millisecond,
		RenderConcurrency: 1, Now: f.currentTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc2.Close)
	got, gotVer, ok := svc2.Current("u1")
	if !ok || gotVer != ver || string(got) != string(want) {
		t.Fatalf("重启后帧 = ver %d ok=%v，期望 ver %d", gotVer, ok, ver)
	}
	if b2.count() != 0 {
		t.Errorf("重启后重新渲染了 %d 次", b2.count())
	}
	if _, _, err := svc2.CurrentForKey("k1", "d1"); err != nil {
		t.Errorf("重启后设备取帧失败：%v", err)
	}
}

func TestCurrentForKey(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.svc.CurrentForKey("k1", "d1"); !errors.Is(err, ErrNoFrame) {
		t.Errorf("还没帧时 = %v，期望 ErrNoFrame", err)
	}
	f.rebuild("u1")
	if _, _, err := f.svc.CurrentForKey("k1", "d1"); err != nil {
		t.Errorf("k1 取帧失败：%v", err)
	}
	for _, c := range []struct {
		name       string
		keyID, dev string
	}{
		{"pending", "kp", "d1"},
		{"disabled", "kx", "d1"},
		{"dev 不符", "ku", "d1"},
		{"没有 user_id", "kn", "d1"},
		{"不存在", "nope", "d1"},
	} {
		if _, _, err := f.svc.CurrentForKey(c.keyID, c.dev); !errors.Is(err, ErrNotPaired) {
			t.Errorf("%s = %v，期望 ErrNotPaired", c.name, err)
		}
	}
}

func TestResend(t *testing.T) {
	f := newFixture(t)
	f.rebuild("u1")
	f.pub.mu.Lock()
	f.pub.sent = nil
	f.pub.mu.Unlock()

	f.svc.Resend("d1")
	if ver, ok := f.pub.last("d1"); !ok || ver != 1 {
		t.Errorf("补发 = %d,%v，期望 ver=1", ver, ok)
	}
	f.svc.Resend("unknown")
	f.svc.Resend("d3") // 配对但还没有帧
	if _, ok := f.pub.last("d3"); ok {
		t.Error("没有帧的设备不该收到补发")
	}
}

func TestDeviceRemovedDropsCache(t *testing.T) {
	f := newFixture(t)
	f.rebuild("u1")
	if _, _, ok := f.svc.Current("u1"); !ok {
		t.Fatal("没有帧")
	}
	f.st.devs["u1"] = nil
	f.svc.DeviceRemoved("u1")
	if _, _, ok := f.svc.Current("u1"); ok {
		t.Error("名下无设备后帧缓存还在")
	}
	if _, err := os.Stat(filepath.Join(f.root, "frames", "u1")); !os.IsNotExist(err) {
		t.Errorf("缓存目录没删：%v", err)
	}
}

func TestPackUnpackRoundTrip(t *testing.T) {
	b := testBundle("x")
	data, err := Pack(b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unpack(data)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Base) != string(b.Base) || string(got.Atlas) != string(b.Atlas) ||
		got.Meta.Frame != b.Meta.Frame {
		t.Errorf("往返后不一致")
	}

	bad := append([]byte(nil), data...)
	bad[packHeader] ^= 0xff // 改底帧第一个字节
	if _, err := Unpack(bad); err == nil {
		t.Error("底帧被改后仍能 Unpack")
	}
	if _, err := Unpack(data[:packHeader-1]); err == nil {
		t.Error("截断的帧包仍能 Unpack")
	}
	badMagic := append([]byte(nil), data...)
	copy(badMagic, "XXXX")
	if _, err := Unpack(badMagic); err == nil {
		t.Error("错误 magic 仍能 Unpack")
	}
}

func TestPackRejectsOverSlotLimit(t *testing.T) {
	b := testBundleN("x", MaxPackBytes)
	if _, err := Pack(b); err == nil {
		t.Fatal("超过 FP_MAX_PACK 仍然打包成功")
	}
}

func TestCorruptCacheIsIgnored(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.root, "frames", "u1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "frame.cache"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := New(f.root, f.st, f.us, f.pub, f.b, Options{DefaultTZ: "UTC"})
	if err != nil {
		t.Fatalf("损坏的缓存不该让 New 失败：%v", err)
	}
	t.Cleanup(svc.Close)
	if _, _, ok := svc.Current("u1"); ok {
		t.Error("损坏的缓存不该被加载")
	}
}
