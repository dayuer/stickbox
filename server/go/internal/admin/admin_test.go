package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/broker"
	"github.com/dayuer/stickbox/server/go/internal/command"
	"github.com/dayuer/stickbox/server/go/internal/store"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

type fakeStore struct {
	keys  []store.Key
	owner string
	bound bool
}

func (f fakeStore) KeysOfDevice(string) ([]store.Key, error) { return f.keys, nil }
func (f fakeStore) OwnerOfDevice(string) (string, bool, error) {
	return f.owner, f.bound, nil
}

type fakeUsage struct {
	rep usage.Report
	at  time.Time
	ok  bool
}

func (f fakeUsage) Latest(string) (usage.Report, time.Time, bool, error) {
	return f.rep, f.at, f.ok, nil
}

type fakeFrames struct {
	ver uint64
	ok  bool
}

func (f fakeFrames) Current(string) ([]byte, uint64, bool) { return nil, f.ver, f.ok }

type fakeRuntime struct{}

func (fakeRuntime) RecentAuth(string, int) []broker.AuthEvent {
	return []broker.AuthEvent{
		{At: time.Unix(100, 0), Dev: "f412fa87c9e0", Code: 5, Result: "invalid_key", Remote: "192.0.2.1:1"},
		{At: time.Unix(99, 0), Dev: "f412fa87c9e0", Code: 0, Result: "ok", Remote: "192.0.2.1:2"},
	}
}
func (fakeRuntime) ConnectedKeys(string) []string { return []string{"key-active"} }
func (fakeRuntime) Pending(string) []command.PendingInfo {
	return []command.PendingInfo{{CID: "c1", Topic: "printer/f412fa87c9e0/usage", Tries: 2}}
}
func (fakeRuntime) RegisterQuota(string) (int, int, time.Time) {
	return 3, 5, time.Unix(200, 0)
}

func newTestHandler() *Handler {
	now := time.Unix(300, 0)
	h := NewHandler(
		fakeStore{
			keys: []store.Key{
				{KeyID: "key-disabled", Dev: "f412fa87c9e0", Role: "device", Disabled: true, Created: 1, Hash: "SECRET-HASH"},
				{KeyID: "key-active", Dev: "f412fa87c9e0", Role: "device", UserID: "u1", Created: 3, Hash: "SECRET-HASH"},
				{KeyID: "key-pending", Dev: "f412fa87c9e0", Role: "device", Pending: true, Created: 2, Hash: "SECRET-HASH"},
			},
			owner: "u1", bound: true,
		},
		fakeUsage{rep: usage.Report{
			FiveHour: &usage.Window{UsedPercentage: 37.5, ResetsAt: 1792244076},
			SevenDay: &usage.Window{UsedPercentage: 19, ResetsAt: 1792413276},
			TZ:       "Asia/Bangkok",
		}, at: time.Unix(250, 0), ok: true},
		fakeFrames{ver: 12, ok: true},
		fakeRuntime{},
	)
	h.now = func() time.Time { return now }
	return h
}

func TestDeviceReport(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/device?dev=f412fa87c9e0&n=5", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rr.Code, rr.Body)
	}
	raw := rr.Body.String()
	if strings.Contains(raw, "SECRET-HASH") || strings.Contains(raw, `"hash"`) {
		t.Errorf("响应泄露了密钥哈希：%s", raw)
	}
	var rep Report
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Dev != "f412fa87c9e0" || !rep.Bound || rep.UserID != "u1" {
		t.Errorf("基础信息 = %+v", rep)
	}
	if len(rep.Keys) != 3 {
		t.Fatalf("keys = %+v", rep.Keys)
	}
	// active 在前，pending 次之，disabled 最后。
	wantStates := []string{"active", "pending", "disabled"}
	for i, want := range wantStates {
		if rep.Keys[i].State != want {
			t.Errorf("第 %d 把 key 状态 = %s，期望 %s", i+1, rep.Keys[i].State, want)
		}
	}
	if len(rep.ConnectedKeys) != 1 || rep.ConnectedKeys[0] != "key-active" {
		t.Errorf("connected = %v", rep.ConnectedKeys)
	}
	if len(rep.RecentAuth) != 2 || rep.RecentAuth[0].Code != 5 {
		t.Errorf("recent auth = %+v", rep.RecentAuth)
	}
	if len(rep.Pending) != 1 || rep.Pending[0].CID != "c1" {
		t.Errorf("pending = %+v", rep.Pending)
	}
	if rep.RegisterQuota.Remaining != 3 || rep.RegisterQuota.Limit != 5 {
		t.Errorf("quota = %+v", rep.RegisterQuota)
	}
	if rep.Usage == nil || rep.Usage.FiveHour == nil || rep.Usage.FiveHour.UsedPercentage != 37.5 ||
		rep.Usage.TZ != "Asia/Bangkok" {
		t.Errorf("usage = %+v", rep.Usage)
	}
	if rep.Frame == nil || !rep.Frame.Available || rep.Frame.Version != 12 {
		t.Errorf("frame = %+v", rep.Frame)
	}
}

func TestDeviceReportLoopbackOnly(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/device?dev=f412fa87c9e0", nil)
	req.RemoteAddr = "203.0.113.1:1234"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("非回环来源 = %d，期望 403", rr.Code)
	}
}

func TestDeviceReportBadDev(t *testing.T) {
	h := newTestHandler()
	for _, dev := range []string{"", "XYZ", "f412fa87c9e"} {
		req := httptest.NewRequest(http.MethodGet, "/device?dev="+dev, nil)
		req.RemoteAddr = "127.0.0.1:1"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("dev=%q status = %d，期望 400", dev, rr.Code)
		}
	}
}

func TestDeviceReportMethodNotAllowed(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/device?dev=f412fa87c9e0", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d，期望 405", rr.Code)
	}
}

func TestServeRejectsNonLoopback(t *testing.T) {
	h := newTestHandler()
	for _, addr := range []string{"0.0.0.0:0", "192.0.2.1:0", ":9091"} {
		if srv, err := Serve(addr, h); err == nil {
			srv.Close()
			t.Errorf("addr=%q 应被拒绝", addr)
		}
	}
}
