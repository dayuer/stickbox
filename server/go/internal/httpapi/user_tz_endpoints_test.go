package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/frame"
	"github.com/dayuer/stickbox/server/go/internal/frames"
	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

func put(h http.Handler, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return do(h, req, hdr)
}

type tzResp struct {
	TZ     string `json:"tz"`
	Source string `json:"source"`
}

func decodeTZ(t *testing.T, rr *httptest.ResponseRecorder) tzResp {
	t.Helper()
	var out tzResp
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON：%s", rr.Body)
	}
	return out
}

// 用户设置 > agent 上报 > default。
func TestUserTZPriority(t *testing.T) {
	h, dep := newTestAPI(t)
	dep.cfg.DefaultTZ = "UTC"
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")

	// 还没有用量：默认。
	rr := get(h, "/api/user/tz", bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("GET = %d %s", rr.Code, rr.Body)
	}
	if out := decodeTZ(t, rr); out.TZ != "UTC" || out.Source != "default" {
		t.Fatalf("无上报时 = %+v", out)
	}

	// agent 上报 Asia/Tokyo。
	body := fmt.Sprintf(`{"five_hour":{"used_percentage":10,"resets_at":%d},"tz":"Asia/Tokyo"}`,
		time.Now().Add(time.Hour).Unix())
	if rr := post(h, "/api/usage", body, bearer(agentTok)); rr.Code != 200 {
		t.Fatalf("上报 = %d %s", rr.Code, rr.Body)
	}
	rr = get(h, "/api/user/tz", bearer(agentTok))
	if out := decodeTZ(t, rr); out.TZ != "Asia/Tokyo" || out.Source != "agent" {
		t.Fatalf("agent 上报后 = %+v", out)
	}

	// 用户设置覆盖 agent。
	rr = put(h, "/api/user/tz", `{"tz":"Asia/Jakarta"}`, bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	if out := decodeTZ(t, rr); out.TZ != "Asia/Jakarta" || out.Source != "user" {
		t.Fatalf("PUT 响应 = %+v", out)
	}
	rr = get(h, "/api/user/tz", bearer(agentTok))
	if out := decodeTZ(t, rr); out.TZ != "Asia/Jakarta" || out.Source != "user" {
		t.Fatalf("GET = %+v", out)
	}

	// 清除后回到 agent。
	rr = put(h, "/api/user/tz", `{"tz":""}`, bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("清除 = %d %s", rr.Code, rr.Body)
	}
	if out := decodeTZ(t, rr); out.TZ != "Asia/Tokyo" || out.Source != "agent" {
		t.Fatalf("清除后 = %+v", out)
	}
	u, _, _ := dep.store.UserByID(mustUserID(t, dep))
	if u.TZ != "" {
		t.Fatalf("库里的时区没清掉：%q", u.TZ)
	}
}

func mustUserID(t *testing.T, dep *testDeps) string {
	t.Helper()
	users, err := dep.store.ListUsers()
	if err != nil || len(users) == 0 {
		t.Fatalf("用户列表 = %+v err=%v", users, err)
	}
	return users[0].ID
}

func TestUserTZRejectsInvalid(t *testing.T) {
	h, dep := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")

	for _, body := range []string{
		`{}`, `{"tz":null}`, `{"tz":"Mars/Olympus"}`, `{"tz":"Local"}`,
		`{"tz":"+07:30"}`, `{"tz":"../etc/passwd"}`,
	} {
		rr := put(h, "/api/user/tz", body, bearer(agentTok))
		if rr.Code != 400 {
			t.Errorf("body=%s → %d %s，期望 400", body, rr.Code, rr.Body)
		}
	}
	u, _, _ := dep.store.UserByID(mustUserID(t, dep))
	if u.TZ != "" {
		t.Fatalf("非法值写进了库：%q", u.TZ)
	}

	// 固定偏移会被规范化成 Etc/GMT。
	if rr := put(h, "/api/user/tz", `{"tz":"+07:00"}`, bearer(agentTok)); rr.Code != 200 {
		t.Fatalf("+07:00 = %d %s", rr.Code, rr.Body)
	} else if out := decodeTZ(t, rr); out.TZ != "Etc/GMT-7" {
		t.Fatalf("+07:00 规范化 = %+v", out)
	}
}

func TestUserTZAuth(t *testing.T) {
	h, dep := newTestAPI(t)
	devTok, code := register(t, h, "f412fa87c9e0")
	agentTok, _, userID := mustPair(t, h, code, "")

	if rr := get(h, "/api/user/tz", nil); rr.Code != 401 {
		t.Errorf("无令牌 = %d", rr.Code)
	}
	if rr := get(h, "/api/user/tz", bearer(devTok)); rr.Code != 401 {
		t.Errorf("device 令牌 = %d", rr.Code)
	}
	// user 角色令牌（目前没有签发入口，但接口约定接受）。
	userTok := issueRoleKey(t, dep, userID, auth.RoleUser)
	if rr := get(h, "/api/user/tz", bearer(userTok)); rr.Code != 200 {
		t.Errorf("user 令牌 = %d %s", rr.Code, rr.Body)
	}
	_ = agentTok
}

func issueRoleKey(t *testing.T, dep *testDeps, userID string, role auth.Role) string {
	t.Helper()
	keyID, secret, token := auth.NewToken()
	hash, err := dep.v.HashSecret(keyID, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := dep.store.InsertKey(store.Key{
		KeyID: keyID, Role: string(role), UserID: userID, Hash: hash,
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

// —— 立即重渲 ——

type recBuilder struct {
	mu   sync.Mutex
	last source.Snapshot
	n    int
}

func (b *recBuilder) Build(s source.Snapshot) (*frame.Bundle, error) {
	b.mu.Lock()
	b.last = s
	b.n++
	b.mu.Unlock()
	base := []byte("base")
	atlas := []byte("atlas")
	bun := &frame.Bundle{Base: base, Atlas: atlas}
	bun.Meta.V = frame.Version
	bs := sha256.Sum256(base)
	as := sha256.Sum256(atlas)
	bun.Meta.Frame = hex.EncodeToString(bs[:])
	bun.Meta.Atlas.SHA256 = hex.EncodeToString(as[:])
	bun.Meta.Atlas.Size = len(atlas)
	return bun, nil
}

func (b *recBuilder) snapshot() source.Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

type recFramePub struct {
	mu   sync.Mutex
	sent map[string][]byte
}

func (p *recFramePub) PublishFrame(dev string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sent == nil {
		p.sent = map[string][]byte{}
	}
	p.sent[dev] = append([]byte(nil), payload...)
	return nil
}

// 改时区必须同步重渲并通知设备，不能等下一次用量上报。
func TestUserTZTriggersImmediateRebuild(t *testing.T) {
	h, dep := newTestAPI(t)
	dep.cfg.DefaultTZ = "UTC"
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, dev, userID := mustPair(t, h, code, "")

	b := &recBuilder{}
	pub := &recFramePub{}
	fs, err := frames.New(dep.cfg.Root, dep.store, dep.api.Usage(), pub, b, frames.Options{
		DefaultTZ: "UTC", Debounce: time.Millisecond, RenderConcurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fs.Close)
	dep.api.SetFrames(fs)

	body := fmt.Sprintf(`{"five_hour":{"used_percentage":10,"resets_at":%d},"tz":"Asia/Tokyo"}`,
		time.Now().Add(time.Hour).Unix())
	if rr := post(h, "/api/usage", body, bearer(agentTok)); rr.Code != 200 {
		t.Fatalf("上报 = %d %s", rr.Code, rr.Body)
	}
	if err := fs.Rebuild(userID); err != nil {
		t.Fatal(err)
	}
	if loc := b.snapshot().Now.Location().String(); loc != "Asia/Tokyo" {
		t.Fatalf("初始时区 = %s", loc)
	}
	_, v1, ok := fs.Current(userID)
	if !ok {
		t.Fatal("没有初始帧")
	}

	rr := put(h, "/api/user/tz", `{"tz":"Pacific/Kiritimati"}`, bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	if loc := b.snapshot().Now.Location().String(); loc != "Pacific/Kiritimati" {
		t.Fatalf("PUT 后没有用新时区重渲：%s", loc)
	}
	_, v2, _ := fs.Current(userID)
	if v2 <= v1 {
		t.Fatalf("帧版本没有前进：v1=%d v2=%d", v1, v2)
	}
	pub.mu.Lock()
	msg := pub.sent[dev]
	pub.mu.Unlock()
	if len(msg) == 0 {
		t.Fatal("没有给设备发 frame 通知")
	}

	// 清除：回到 agent 上报的 Asia/Tokyo。
	if rr := put(h, "/api/user/tz", `{"tz":""}`, bearer(agentTok)); rr.Code != 200 {
		t.Fatalf("清除 = %d %s", rr.Code, rr.Body)
	}
	if loc := b.snapshot().Now.Location().String(); loc != "Asia/Tokyo" {
		t.Fatalf("清除后没有回退 agent 时区：%s", loc)
	}
}
