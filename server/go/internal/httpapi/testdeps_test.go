package httpapi

import (
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

// recPub 记下推给每台设备的最后一份用量和配对通知。
type recPub struct {
	usage map[string][]byte
	pair  map[string][]byte
}

func (p *recPub) PublishUsage(dev string, payload []byte) error {
	if p.usage == nil {
		p.usage = map[string][]byte{}
	}
	p.usage[dev] = payload
	return nil
}

func (p *recPub) PublishPair(dev string, payload []byte) error {
	if p.pair == nil {
		p.pair = map[string][]byte{}
	}
	p.pair[dev] = payload
	return nil
}

func (p *recPub) KickDevice(dev string) {} // 测试里没有真连接可踢

type testDeps struct {
	store *store.Store
	reg   *registry.Registry
	v     *auth.Verifier
	cfg   *config.Config
	pub   *recPub
	api   *API // 用于在 New 之后注入帧服务
}

func newTestAPI(t *testing.T) (http.Handler, *testDeps) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	dep := &testDeps{store: st, pub: &recPub{}}
	reg := registry.New(registry.Options{IdleTimeout: time.Hour})
	t.Cleanup(reg.Shutdown)
	dep.reg = reg
	dep.v = auth.NewVerifier(st)
	dep.cfg = &config.Config{Root: dir}
	dep.api = New(dep.cfg, st, dep.v, reg, dep.pub)
	return dep.api.Handler(), dep
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

// register 调 POST /api/device/register，返回待配对设备密钥和配对码。
func register(t *testing.T, h http.Handler, dev string) (token, code string) {
	t.Helper()
	rr := post(h, "/api/device/register", `{"dev":"`+dev+`"}`, nil)
	if rr.Code != 200 {
		t.Fatalf("register = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		Token string `json:"token"`
		Code  string `json:"code"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Token == "" || len(out.Code) != 6 {
		t.Fatalf("register 响应异常：%s", rr.Body)
	}
	return out.Token, out.Code
}

func tryPair(h http.Handler, code, agentToken string) *httptest.ResponseRecorder {
	hdr := map[string]string{"Content-Type": "application/json"}
	if agentToken != "" {
		hdr = bearer(agentToken)
	}
	return post(h, "/api/agent/pair", `{"code":"`+code+`","name":"test-mac"}`, hdr)
}

// mustPair 配对并返回 agent 令牌、dev、user_id。
func mustPair(t *testing.T, h http.Handler, code, agentToken string) (token, dev, userID string) {
	t.Helper()
	rr := tryPair(h, code, agentToken)
	if rr.Code != 200 {
		t.Fatalf("pair = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		Token  string `json:"token"`
		Dev    string `json:"dev"`
		UserID string `json:"user_id"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	return out.Token, out.Dev, out.UserID
}

func agentDevices(t *testing.T, h http.Handler, tok string) []struct {
	Dev    string `json:"dev"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
} {
	t.Helper()
	rr := get(h, "/api/agent/devices", bearer(tok))
	if rr.Code != 200 {
		t.Fatalf("devices = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		Devices []struct {
			Dev    string `json:"dev"`
			Name   string `json:"name"`
			Online bool   `json:"online"`
		} `json:"devices"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	return out.Devices
}
