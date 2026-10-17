package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/config"
	"github.com/dayuer/stickbox/server/go/internal/registry"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

type fakeSender struct{ last string }

func (f *fakeSender) Send(ctx context.Context, phone, code string) error {
	f.last = code
	return nil
}

// recPub 记下推给每台设备的最后一份用量，测试据此断言。
type recPub struct{ usage map[string][]byte }

func (p *recPub) PublishUsage(dev string, payload []byte) error {
	if p.usage == nil {
		p.usage = map[string][]byte{}
	}
	p.usage[dev] = payload
	return nil
}

type nopMem struct{}

func (nopMem) Invalidate(string) {}

type testDeps struct {
	store  *store.Store
	phone  *auth.PhoneBox
	sender *fakeSender
	reg    *registry.Registry
	v      *auth.Verifier
	cfg    *config.Config
	pub    *recPub
	clock  time.Time
}

func newTestAPI(t *testing.T) (http.Handler, *testDeps) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	pb, err := auth.NewPhoneBox("test-pepper",
		"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatal(err)
	}
	dep := &testDeps{store: st, phone: pb, sender: &fakeSender{},
		clock: time.Unix(1_000_000, 0)}
	sms := auth.NewSMS(st, dep.sender, func() time.Time { return dep.clock })
	dep.pub = &recPub{}
	reg := registry.New(registry.Options{IdleTimeout: time.Hour})
	t.Cleanup(reg.Shutdown)
	dep.reg = reg
	dep.v = auth.NewVerifier(st)

	dep.cfg = &config.Config{Root: dir}
	return New(dep.cfg, st, dep.v, pb, sms, reg, nopMem{}, dep.pub).Handler(), dep
}

// —— 请求辅助 ——

func bearer(tok string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tok}
}

func do(h http.Handler, req *http.Request, hdr map[string]string) *httptest.ResponseRecorder {
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func post(h http.Handler, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return do(h, req, hdr)
}

func get(h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	return do(h, httptest.NewRequest("GET", path, nil), hdr)
}

// login 走完整的发码 → 校验流程，返回 app session token。
func login(t *testing.T, h http.Handler, dep *testDeps, phone string) string {
	t.Helper()
	if rr := post(h, "/api/auth/sms", `{"phone":"`+phone+`"}`, nil); rr.Code != 200 {
		t.Fatalf("发码失败：%d %s", rr.Code, rr.Body)
	}
	rr := post(h, "/api/auth/verify",
		`{"phone":"`+phone+`","code":"`+dep.sender.last+`","device":"test"}`, nil)
	var out struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Token == "" {
		t.Fatalf("登录失败：%d %s", rr.Code, rr.Body)
	}
	return out.Token
}

// agentLogin 为已有账号签发一把 agent 令牌。
// 发码复用 /api/auth/sms；agent 登录不会新建账号。
func agentLogin(t *testing.T, h http.Handler, dep *testDeps, phone string) string {
	t.Helper()
	// 同一号码 60 秒内不能重发；测试时钟往前拨，绕开短信闸。
	dep.clock = dep.clock.Add(2 * time.Minute)
	if rr := post(h, "/api/auth/sms", `{"phone":"`+phone+`"}`, nil); rr.Code != 200 {
		t.Fatalf("发码失败：%d %s", rr.Code, rr.Body)
	}
	rr := post(h, "/api/agent/login",
		`{"phone":"`+phone+`","code":"`+dep.sender.last+`","name":"mac"}`, nil)
	var out struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Token == "" {
		t.Fatalf("agent 登录失败：%d %s", rr.Code, rr.Body)
	}
	return out.Token
}

// enroll 绑一台设备，返回 device 密钥。
func enroll(t *testing.T, h http.Handler, token, dev string) string {
	t.Helper()
	rr := post(h, "/api/device/enroll", `{"dev":"`+dev+`"}`, bearer(token))
	if rr.Code != 200 {
		t.Fatalf("enroll 失败：%d %s", rr.Code, rr.Body)
	}
	var out struct {
		DeviceKey string `json:"device_key"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	return out.DeviceKey
}
