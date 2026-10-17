package frames

import (
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
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

// —— 假的依赖 ——

type fakeStore struct {
	devs   map[string][]string
	owners map[string]string
	err    error
}

func (f *fakeStore) DevicesOfUser(userID string) ([]string, error) {
	return f.devs[userID], f.err
}

func (f *fakeStore) OwnerOfDevice(dev string) (string, bool, error) {
	u, ok := f.owners[dev]
	return u, ok, f.err
}

type fakeUsage struct {
	mu      sync.Mutex
	reports map[string]usage.Report
	at      time.Time
	ok      bool
	err     error
}

func (f *fakeUsage) Latest(userID string) (usage.Report, time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil || !f.ok {
		return usage.Report{}, time.Time{}, false, f.err
	}
	if r, ok := f.reports[userID]; ok {
		return r, f.at, true, nil
	}
	return usage.Report{}, time.Time{}, false, nil
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

type fakeBuilder struct {
	mu      sync.Mutex
	calls   int
	failErr error
	payload string
}

func (f *fakeBuilder) Build(s source.Snapshot) (*frame.Bundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failErr != nil {
		return nil, f.failErr
	}
	p := f.payload
	if p == "" && len(s.Windows) > 0 {
		p = fmt.Sprintf("%.0f", s.Windows[0].UsedPercentage*100)
	}
	return testBundle(p), nil
}

func (f *fakeBuilder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func testBundle(payload string) *frame.Bundle {
	base := []byte("base:" + payload)
	atlas := []byte("atlas:" + payload)
	b := &frame.Bundle{Base: base, Atlas: atlas}
	bs := sha256.Sum256(base)
	as := sha256.Sum256(atlas)
	b.Meta.Frame = hex.EncodeToString(bs[:])
	b.Meta.Atlas.SHA256 = hex.EncodeToString(as[:])
	b.Meta.Atlas.Size = len(atlas)
	return b
}

type fixture struct {
	t    *testing.T
	root string
	svc  *Service
	st   *fakeStore
	us   *fakeUsage
	pub  *fakePub
	b    *fakeBuilder
	now  time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t:    t,
		root: t.TempDir(),
		st: &fakeStore{
			devs:   map[string][]string{"u1": {"d1", "d2"}, "u2": {"d3"}},
			owners: map[string]string{"d1": "u1", "d2": "u1", "d3": "u2"},
		},
		us: &fakeUsage{
			ok: true,
			at: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
			reports: map[string]usage.Report{
				"u1": {FiveHour: &usage.Window{UsedPercentage: 42, ResetsAt: 1792228800}},
				"u2": {FiveHour: &usage.Window{UsedPercentage: 7, ResetsAt: 1792228800}},
			},
		},
		pub: &fakePub{},
		b:   &fakeBuilder{},
		now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
	}
	svc, err := New(f.root, f.st, f.us, f.pub, f.b, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
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

	f.b.mu.Lock()
	f.b.failErr = errors.New("band cap exceeded")
	f.b.mu.Unlock()

	if err := f.svc.Rebuild("u1"); err == nil {
		t.Fatal("构建失败应返回错误")
	}
	now, ver, ok := f.svc.Current("u1")
	if !ok || ver != oldVer || string(now) != string(oldCopy) {
		t.Errorf("构建失败后帧变了：ver %d→%d", oldVer, ver)
	}
	if got := f.b.count(); got != 2 {
		t.Errorf("builder 调用 = %d，期望 2", got)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	f := newFixture(t)
	f.rebuild("u1")
	want, ver, _ := f.svc.Current("u1")

	// 新进程：同一个 root，builder 一旦被调用就说明没有从磁盘恢复。
	b2 := &fakeBuilder{failErr: errors.New("不该重新渲染")}
	svc2, err := New(f.root, f.st, f.us, f.pub, b2, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	got, gotVer, ok := svc2.Current("u1")
	if !ok || gotVer != ver || string(got) != string(want) {
		t.Fatalf("重启后帧 = ver %d ok=%v，期望 ver %d", gotVer, ok, ver)
	}
	if b2.count() != 0 {
		t.Errorf("重启后重新渲染了 %d 次", b2.count())
	}
	if _, _, err := svc2.CurrentForDevice("d1"); err != nil {
		t.Errorf("重启后设备取帧失败：%v", err)
	}
}

func TestCurrentForDevice(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.svc.CurrentForDevice("d1"); !errors.Is(err, ErrNoFrame) {
		t.Errorf("还没帧时 = %v，期望 ErrNoFrame", err)
	}
	f.rebuild("u1")
	if _, _, err := f.svc.CurrentForDevice("d1"); err != nil {
		t.Errorf("d1 取帧失败：%v", err)
	}
	if _, _, err := f.svc.CurrentForDevice("unknown"); !errors.Is(err, ErrNotPaired) {
		t.Errorf("未配对设备 = %v，期望 ErrNotPaired", err)
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

func TestRebuildStaleAfterMidnight(t *testing.T) {
	f := newFixture(t)
	f.rebuild("u1")
	f.rebuild("u2")
	before := f.b.count()

	f.now = f.now.Add(24 * time.Hour)
	f.svc.rebuildStale()

	if got := f.version("u1"); got != 2 {
		t.Errorf("u1 跨天后版本 = %d，期望 2", got)
	}
	if got := f.version("u2"); got != 2 {
		t.Errorf("u2 跨天后版本 = %d，期望 2", got)
	}
	if got := f.b.count() - before; got != 2 {
		t.Errorf("跨天重渲染 = %d 次，期望 2", got)
	}

	// 同一天再跑一次不该重渲染。
	before = f.b.count()
	f.svc.rebuildStale()
	if got := f.b.count() - before; got != 0 {
		t.Errorf("同一天重复重渲染 %d 次", got)
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

func TestCorruptCacheIsIgnored(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.root, "frames", "u1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "frame.cache"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := New(f.root, f.st, f.us, f.pub, f.b, func() time.Time { return f.now })
	if err != nil {
		t.Fatalf("损坏的缓存不该让 New 失败：%v", err)
	}
	if _, _, ok := svc.Current("u1"); ok {
		t.Error("损坏的缓存不该被加载")
	}
}
