package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeAgentToken = "agt_SECRET_TOKEN_0123456789abcdef"

// fakeStickbox 是伪造的服务端：只实现 agent 用到的 pair / devices / unpair。
type fakeStickbox struct {
	*httptest.Server
	mu         sync.Mutex
	pairCalls  int
	validCode  string
	pairStatus int    // 非 0 时强制返回该状态（错误体 {"e":pairErr}）
	pairErr    string //
	devName    string
	unpaired   []string
}

func newFakeStickbox(t *testing.T) *fakeStickbox {
	t.Helper()
	f := &fakeStickbox{validCode: "123456", devName: "客厅电视"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/pair", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pairCalls++
		var in struct{ Code, Name string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if f.pairStatus != 0 {
			w.WriteHeader(f.pairStatus)
			_, _ = w.Write([]byte(`{"e":"` + f.pairErr + `"}`))
			return
		}
		if in.Code != f.validCode {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"e":"bad code"}`))
			return
		}
		out := map[string]string{"dev": "dev-abc123"}
		if r.Header.Get("Authorization") == "" {
			out["token"] = fakeAgentToken
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/api/agent/devices", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeAgentToken {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"e":"unauthorized"}`))
			return
		}
		f.mu.Lock()
		name := f.devName
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"devices": []deviceRow{
			{Dev: "dev-abc123", Name: name, Online: true, Bound: 1700000000},
		}})
	})
	mux.HandleFunc("/api/agent/unpair", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeAgentToken {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"e":"unauthorized"}`))
			return
		}
		var in struct{ Dev string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.unpaired = append(f.unpaired, in.Dev)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeStickbox) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pairCalls
}

// logBuf 收集 uiServer 的日志，用来断言日志里没有令牌。
type logBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (l *logBuf) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.sb, f+"\n", a...)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sb.String()
}

type apiEnv struct {
	s    *uiServer
	ts   *httptest.Server
	fake *fakeStickbox
	logs *logBuf
}

func newAPIEnv(t *testing.T) *apiEnv {
	t.Helper()
	s, ts := newTestUI(t)
	fake := newFakeStickbox(t)
	logs := &logBuf{}
	s.logf = logs.logf
	s.opts.server = fake.URL
	return &apiEnv{s: s, ts: ts, fake: fake, logs: logs}
}

func (e *apiEnv) get(t *testing.T, path string) (int, string) {
	t.Helper()
	code, body, _ := do(t, e.ts, "GET", path, map[string]string{"X-Stickbox-Token": e.s.token}, "")
	return code, body
}

func (e *apiEnv) post(t *testing.T, path, body string) (int, string) {
	t.Helper()
	code, out, _ := do(t, e.ts, "POST", path, map[string]string{
		"X-Stickbox-Token": e.s.token,
		"Origin":           e.ts.URL,
		"Content-Type":     "application/json",
	}, body)
	return code, out
}

func (e *apiEnv) pairOK(t *testing.T) {
	t.Helper()
	if code, body := e.post(t, "/api/pair", `{"code":"123456"}`); code != 200 {
		t.Fatalf("配对失败 %d %s", code, body)
	}
}

func TestUIAPIRequiresToken(t *testing.T) {
	e := newAPIEnv(t)
	routes := []struct{ method, path string }{
		{"GET", "/api/status"}, {"GET", "/api/devices"},
		{"POST", "/api/pair"}, {"POST", "/api/unpair"}, {"POST", "/api/run-once"},
	}
	for _, rt := range routes {
		hdr := map[string]string{"Origin": e.ts.URL, "Content-Type": "application/json"}
		if code, _, _ := do(t, e.ts, rt.method, rt.path, hdr, `{"code":"123456"}`); code != http.StatusUnauthorized {
			t.Errorf("%s %s 无 token = %d，期望 401", rt.method, rt.path, code)
		}
		hdr["X-Stickbox-Token"] = "wrong"
		if code, _, _ := do(t, e.ts, rt.method, rt.path, hdr, `{"code":"123456"}`); code != http.StatusUnauthorized {
			t.Errorf("%s %s 错 token = %d，期望 401", rt.method, rt.path, code)
		}
	}
	// 用 URL 里的 ?t= 不能代替头
	if code, _, _ := do(t, e.ts, "GET", "/api/status?t="+e.s.token, nil, ""); code != http.StatusUnauthorized {
		t.Errorf("API 不接受 query token，得到 %d", code)
	}
	if e.fake.calls() != 0 {
		t.Error("未授权请求不该打到服务端")
	}
}

func TestUIAPIRejectsForeignHostAndOrigin(t *testing.T) {
	e := newAPIEnv(t)
	hdr := func(m map[string]string) map[string]string {
		m["X-Stickbox-Token"] = e.s.token
		m["Content-Type"] = "application/json"
		return m
	}
	if code, _, _ := do(t, e.ts, "GET", "/api/status", hdr(map[string]string{"Host": "evil.example.com"}), ""); code != 403 {
		t.Errorf("外部 Host = %d，期望 403", code)
	}
	// 带着正确 token 的跨站 POST（CSRF）
	for _, o := range []string{"http://evil.example.com", "http://127.0.0.1:1", "null"} {
		code, _, _ := do(t, e.ts, "POST", "/api/pair", hdr(map[string]string{"Origin": o}), `{"code":"123456"}`)
		if code != 403 {
			t.Errorf("Origin=%s POST = %d，期望 403", o, code)
		}
	}
	// 缺 Origin 的 POST
	if code, _, _ := do(t, e.ts, "POST", "/api/pair", hdr(map[string]string{}), `{"code":"123456"}`); code != 403 {
		t.Errorf("缺 Origin 的 POST = %d，期望 403", code)
	}
	if e.fake.calls() != 0 {
		t.Error("被拒绝的请求不该打到服务端")
	}
}

func TestUIAPIPostNeedsJSONContentType(t *testing.T) {
	e := newAPIEnv(t)
	code, _, _ := do(t, e.ts, "POST", "/api/pair", map[string]string{
		"X-Stickbox-Token": e.s.token, "Origin": e.ts.URL, "Content-Type": "text/plain",
	}, `{"code":"123456"}`)
	if code != http.StatusUnsupportedMediaType {
		t.Errorf("= %d，期望 415", code)
	}
}

func TestUIAPIMethodNotAllowed(t *testing.T) {
	e := newAPIEnv(t)
	code, _, _ := do(t, e.ts, "POST", "/api/status", map[string]string{
		"X-Stickbox-Token": e.s.token, "Origin": e.ts.URL, "Content-Type": "application/json",
	}, `{}`)
	if code != http.StatusMethodNotAllowed {
		t.Errorf("= %d，期望 405", code)
	}
}

func TestUIPairSuccessSavesTokenButNeverLeaksIt(t *testing.T) {
	e := newAPIEnv(t)
	code, body := e.post(t, "/api/pair", `{"code":" 123456 ","name":"我的电脑"}`)
	if code != 200 || !strings.Contains(body, `"ok":true`) || !strings.Contains(body, "dev-abc123") {
		t.Fatalf("pair = %d %s", code, body)
	}
	if !strings.Contains(body, "电视会在几秒内切到看板") {
		t.Errorf("成功文案缺失：%s", body)
	}
	// 令牌确实落盘了（与 CLI 同一路径）……
	c, err := loadConfig()
	if err != nil || c.Token != fakeAgentToken || c.Name != "我的电脑" || c.PairedAt == 0 {
		t.Fatalf("配置 = %+v err=%v", c, err)
	}
	// ……但所有响应与日志里都没有它。
	_, st := e.get(t, "/api/status")
	_, dv := e.get(t, "/api/devices")
	all := body + st + dv + e.logs.String()
	if strings.Contains(all, fakeAgentToken) || strings.Contains(all, "SECRET_TOKEN") {
		t.Fatalf("响应/日志里出现了令牌：%s", all)
	}
	if !strings.Contains(st, `"paired":true`) {
		t.Errorf("status 应显示已配对：%s", st)
	}
	// 配对成功要触发 60 秒退出计时
	select {
	case <-e.s.paired:
	default:
		t.Error("配对成功应通知退出计时")
	}
}

func TestUIPairBadFormat(t *testing.T) {
	e := newAPIEnv(t)
	for _, b := range []string{`{"code":"12345"}`, `{"code":"abcdef"}`, `{"code":"1234567"}`, `{}`, `not json`} {
		if code, _ := e.post(t, "/api/pair", b); code != 400 {
			t.Errorf("%s → %d，期望 400", b, code)
		}
	}
	if e.fake.calls() != 0 {
		t.Error("格式错误不该打到服务端")
	}
}

func TestUIPairWrongOrExpiredCode(t *testing.T) {
	e := newAPIEnv(t)
	code, body := e.post(t, "/api/pair", `{"code":"000000"}`)
	if code != 400 || !strings.Contains(body, `"e":"bad_code"`) {
		t.Fatalf("= %d %s", code, body)
	}
	if !strings.Contains(body, "每 10 分钟换新") || !strings.Contains(body, "最新的码") {
		t.Errorf("缺少过期提示：%s", body)
	}
	if c, _ := loadConfig(); c.Token != "" {
		t.Error("失败不应保存令牌")
	}
}

func TestUIPairNetworkFailure(t *testing.T) {
	e := newAPIEnv(t)
	e.fake.Close() // 服务端不可达
	code, body := e.post(t, "/api/pair", `{"code":"123456"}`)
	if code != http.StatusBadGateway || !strings.Contains(body, `"e":"network"`) || !strings.Contains(body, "连不上服务器") {
		t.Fatalf("= %d %s", code, body)
	}
	// 网络失败不是猜码，不计入限速
	e.s.failMu.Lock()
	n := len(e.s.failures)
	e.s.failMu.Unlock()
	if n != 0 {
		t.Errorf("网络失败不该计入限速，计了 %d", n)
	}
}

func TestUIPairServerSideRateLimited(t *testing.T) {
	e := newAPIEnv(t)
	e.fake.pairStatus, e.fake.pairErr = 429, "rate limited"
	code, body := e.post(t, "/api/pair", `{"code":"123456"}`)
	if code != 429 || !strings.Contains(body, `"e":"rate_limited"`) {
		t.Fatalf("= %d %s", code, body)
	}
}

func TestUIPairFailureRateLimit(t *testing.T) {
	e := newAPIEnv(t)
	now := time.Unix(1_800_000_000, 0)
	e.s.now = func() time.Time { return now }

	for i := 0; i < uiPairFailLimit; i++ {
		if code, _ := e.post(t, "/api/pair", `{"code":"000000"}`); code != 400 {
			t.Fatalf("第 %d 次失败应为 400，得到 %d", i+1, code)
		}
	}
	calls := e.fake.calls()
	// 第 6 次：本地直接限速，不再打服务端；连正确的码也一样。
	for _, b := range []string{`{"code":"000000"}`, `{"code":"123456"}`} {
		code, body := e.post(t, "/api/pair", b)
		if code != 429 || !strings.Contains(body, `"retry_after"`) {
			t.Fatalf("限速 = %d %s", code, body)
		}
	}
	if e.fake.calls() != calls {
		t.Errorf("限速期间不该再打服务端：%d → %d", calls, e.fake.calls())
	}
	// 窗口过去后恢复，正确的码可以配上。
	now = now.Add(uiPairFailWindow + time.Second)
	if code, body := e.post(t, "/api/pair", `{"code":"123456"}`); code != 200 {
		t.Fatalf("窗口过后应恢复：%d %s", code, body)
	}
}

func TestUIDevicesAndUnpair(t *testing.T) {
	e := newAPIEnv(t)
	// 未配对：409
	if code, body := e.get(t, "/api/devices"); code != 409 || !strings.Contains(body, "not_paired") {
		t.Fatalf("未配对 devices = %d %s", code, body)
	}
	e.pairOK(t)
	code, body := e.get(t, "/api/devices")
	if code != 200 || !strings.Contains(body, "dev-abc123") || !strings.Contains(body, "客厅电视") {
		t.Fatalf("devices = %d %s", code, body)
	}
	if code, body := e.post(t, "/api/unpair", `{"dev":"dev-abc123"}`); code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("unpair = %d %s", code, body)
	}
	e.fake.mu.Lock()
	got := append([]string(nil), e.fake.unpaired...)
	e.fake.mu.Unlock()
	if len(got) != 1 || got[0] != "dev-abc123" {
		t.Errorf("服务端收到的解绑 = %v", got)
	}
	if code, _ := e.post(t, "/api/unpair", `{"dev":""}`); code != 400 {
		t.Errorf("空 dev = %d，期望 400", code)
	}
}

// 即使服务端把令牌回显进某个字段，出站前也会被抹掉（兜底）。
func TestUIScrubsTokenEchoedByServer(t *testing.T) {
	e := newAPIEnv(t)
	e.pairOK(t)
	e.fake.mu.Lock()
	e.fake.devName = "屏幕-" + fakeAgentToken
	e.fake.mu.Unlock()
	code, body := e.get(t, "/api/devices")
	if code != 200 {
		t.Fatalf("= %d %s", code, body)
	}
	if strings.Contains(body, fakeAgentToken) || !strings.Contains(body, "***") {
		t.Errorf("令牌应被抹掉：%s", body)
	}
}

func TestUIStatusUnpairedAndWithState(t *testing.T) {
	e := newAPIEnv(t)
	code, body := e.get(t, "/api/status")
	if code != 200 || !strings.Contains(body, `"paired":false`) || !strings.Contains(body, e.fake.URL) {
		t.Fatalf("= %d %s", code, body)
	}
	if err := saveState(agentState{LastSuccessAt: 1700000000, LastError: "boom", LastErrorAt: 1700000100,
		ClaudeAuth: stateAuthIn, ConsecutiveFailures: 2, NextRetryAt: 1700000900, Cooldown: true}); err != nil {
		t.Fatal(err)
	}
	_, body = e.get(t, "/api/status")
	var st map[string]any
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st["last_error"] != "boom" || st["cooldown"] != true || st["claude_auth"] != "logged_in" || st["consecutive_failures"] != float64(2) {
		t.Errorf("status = %v", st)
	}
	if _, has := st["token"]; has {
		t.Error("status 不该有 token 字段")
	}
}

func TestUIRunOnce(t *testing.T) {
	e := newAPIEnv(t)
	calls := 0
	e.s.runRound = func(c *agentConfig, logf func(string, ...any)) error { calls++; return nil }
	// 未配对：不执行
	if code, _ := e.post(t, "/api/run-once", `{}`); code != 409 || calls != 0 {
		t.Fatalf("未配对 run-once = %d calls=%d", code, calls)
	}
	e.pairOK(t)
	if code, body := e.post(t, "/api/run-once", `{}`); code != 200 || calls != 1 {
		t.Fatalf("run-once = %d %s calls=%d", code, body, calls)
	}
	e.s.runRound = func(*agentConfig, func(string, ...any)) error { return &skipError{reason: "限额为空"} }
	if code, body := e.post(t, "/api/run-once", `{}`); code != 200 || !strings.Contains(body, `"skipped":true`) {
		t.Fatalf("skip = %d %s", code, body)
	}
	e.s.runRound = func(*agentConfig, func(string, ...any)) error { return errors.New("上报失败：boom") }
	if code, body := e.post(t, "/api/run-once", `{}`); code != 502 || !strings.Contains(body, "boom") {
		t.Fatalf("失败 = %d %s", code, body)
	}
}

// run-once 串行：上一轮没结束时第二个请求得到 409，runRound 不会并发进入。
func TestUIRunOnceNeverOverlaps(t *testing.T) {
	e := newAPIEnv(t)
	e.pairOK(t)
	enter := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	active, maxActive := 0, 0
	e.s.runRound = func(*agentConfig, func(string, ...any)) error {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		enter <- struct{}{}
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		return nil
	}
	done := make(chan int, 1)
	go func() { c, _ := e.post(t, "/api/run-once", `{}`); done <- c }()
	<-enter
	if code, body := e.post(t, "/api/run-once", `{}`); code != 409 || !strings.Contains(body, "busy") {
		t.Fatalf("重叠请求 = %d %s", code, body)
	}
	close(release)
	if c := <-done; c != 200 {
		t.Fatalf("第一个请求 = %d", c)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Errorf("同时运行了 %d 轮", maxActive)
	}
}
