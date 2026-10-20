package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestUI 起一个真实监听的 uiServer（httptest 绑 127.0.0.1），
// 配置目录指向临时目录，避免碰用户真实配置。
func newTestUI(t *testing.T) (*uiServer, *httptest.Server) {
	t.Helper()
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	s, err := newUIServer(uiOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.logf = func(string, ...any) {}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	_, p, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	s.port, _ = strconv.Atoi(p)
	return s, ts
}

// do 发请求；host/origin 为空串表示用默认值，"-" 表示不带 Origin。
func do(t *testing.T, ts *httptest.Server, method, path string, hdr map[string]string, body string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestUIIndexNeedsToken(t *testing.T) {
	s, ts := newTestUI(t)
	if code, _, _ := do(t, ts, "GET", "/", nil, ""); code != http.StatusUnauthorized {
		t.Errorf("无 token = %d，期望 401", code)
	}
	if code, _, _ := do(t, ts, "GET", "/?t=wrong", nil, ""); code != http.StatusUnauthorized {
		t.Errorf("错 token = %d，期望 401", code)
	}
	code, body, h := do(t, ts, "GET", "/?t="+s.token, nil, "")
	if code != http.StatusOK || !strings.Contains(body, "StickBox") {
		t.Errorf("正确 token = %d %q", code, body)
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "http") {
		t.Errorf("CSP 应只允许同源：%q", csp)
	}
	if h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" {
		t.Errorf("缺安全头：%v", h)
	}
}

func TestUIRejectsForeignHost(t *testing.T) {
	s, ts := newTestUI(t)
	for _, host := range []string{"evil.example.com", "evil.example.com:" + strconv.Itoa(s.port), "127.0.0.1", "127.0.0.1:1", "192.168.1.5:" + strconv.Itoa(s.port)} {
		code, _, _ := do(t, ts, "GET", "/?t="+s.token, map[string]string{"Host": host}, "")
		if code != http.StatusForbidden {
			t.Errorf("Host=%s → %d，期望 403", host, code)
		}
	}
	// localhost + 实际端口也算本机
	if code, _, _ := do(t, ts, "GET", "/?t="+s.token, map[string]string{"Host": "localhost:" + strconv.Itoa(s.port)}, ""); code != http.StatusOK {
		t.Errorf("localhost 应放行，得到 %d", code)
	}
}

func TestUIRejectsForeignOrigin(t *testing.T) {
	s, ts := newTestUI(t)
	for _, o := range []string{"http://evil.example.com", "https://127.0.0.1:" + strconv.Itoa(s.port), "null"} {
		code, _, _ := do(t, ts, "GET", "/?t="+s.token, map[string]string{"Origin": o}, "")
		if code != http.StatusForbidden {
			t.Errorf("Origin=%s → %d，期望 403", o, code)
		}
	}
	// POST 不带 Origin 同样拒绝
	if code, _, _ := do(t, ts, "POST", "/", nil, ""); code != http.StatusForbidden {
		t.Errorf("POST 无 Origin = %d，期望 403", code)
	}
}

func TestUIStaticAssetsNeedLocalHostOnly(t *testing.T) {
	_, ts := newTestUI(t)
	if code, _, _ := do(t, ts, "GET", "/app.js", map[string]string{"Host": "evil.example.com"}, ""); code != http.StatusForbidden {
		t.Errorf("静态资源也要校验 Host，得到 %d", code)
	}
}

// 绑定地址只能是 127.0.0.1，不能是 0.0.0.0 / ::。
func TestListenLoopbackOnly(t *testing.T) {
	ln, err := listenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)
	if !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) || addr.IP.IsUnspecified() {
		t.Fatalf("监听地址 = %s，期望 127.0.0.1", addr)
	}
	if !strings.HasPrefix(ln.Addr().String(), "127.0.0.1:") {
		t.Fatalf("监听地址 = %s", ln.Addr())
	}
}

func TestNewUIServerTokenRandom(t *testing.T) {
	a, _ := newUIServer(uiOptions{})
	b, _ := newUIServer(uiOptions{})
	if len(a.token) < 32 || a.token == b.token {
		t.Fatalf("token 应足够长且每次不同：%q %q", a.token, b.token)
	}
}

func serveForTest(t *testing.T, s *uiServer, ctx context.Context) <-chan string {
	t.Helper()
	ln, err := listenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	s.port = ln.Addr().(*net.TCPAddr).Port
	out := make(chan string, 1)
	go func() {
		r, err := s.serve(ctx, ln)
		if err != nil {
			r = "error: " + err.Error()
		}
		out <- r
	}()
	return out
}

func waitReason(t *testing.T, ch <-chan string, within time.Duration) string {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatal("服务没有按时退出")
		return ""
	}
}

func TestUIIdleExit(t *testing.T) {
	s, _ := newUIServer(uiOptions{})
	s.idle = 150 * time.Millisecond
	ch := serveForTest(t, s, context.Background())
	if r := waitReason(t, ch, 3*time.Second); !strings.Contains(r, "空闲") {
		t.Errorf("退出原因 = %q", r)
	}
}

// 有活动就不退出：持续访问超过 idle 总时长后仍在线。
func TestUIActivityDefersIdleExit(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	s, _ := newUIServer(uiOptions{})
	s.idle = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := serveForTest(t, s, ctx)
	url := "http://127.0.0.1:" + strconv.Itoa(s.port) + "/app.js"
	for i := 0; i < 6; i++ {
		time.Sleep(100 * time.Millisecond)
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("第 %d 次访问失败（不该已退出）：%v", i, err)
		}
		resp.Body.Close()
	}
	select {
	case r := <-ch:
		t.Fatalf("有活动时不该退出：%s", r)
	default:
	}
	cancel()
	waitReason(t, ch, 3*time.Second)
}

func TestUIExitsAfterPair(t *testing.T) {
	s, _ := newUIServer(uiOptions{})
	s.idle = time.Hour
	s.afterPair = 150 * time.Millisecond
	ch := serveForTest(t, s, context.Background())
	s.markPaired()
	if r := waitReason(t, ch, 3*time.Second); !strings.Contains(r, "配对已完成") {
		t.Errorf("退出原因 = %q", r)
	}
}

func TestUIExitsOnContextCancel(t *testing.T) {
	s, _ := newUIServer(uiOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	ch := serveForTest(t, s, ctx)
	cancel()
	waitReason(t, ch, 3*time.Second)
}
