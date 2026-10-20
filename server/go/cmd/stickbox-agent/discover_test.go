package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func allowAny(net.IP) bool { return true }

// fakeBoardUDP 伪造板子：收到 SBPAIR?1 就向来源单播 replies 里的每一条。
func fakeBoardUDP(t *testing.T, replies ...string) (addr string, probes func() []string) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var mu sync.Mutex
	var got []string
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, string(buf[:n]))
			mu.Unlock()
			for _, r := range replies {
				_, _ = conn.WriteToUDP([]byte(r), from)
			}
		}
	}()
	return conn.LocalAddr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func testUDP(addr string, accept func(net.IP) bool) *udpDiscoverer {
	return &udpDiscoverer{addr: addr, wait: 300 * time.Millisecond, accept: accept}
}

const (
	replyA = `{"v":1,"dev":"aabbccddeeff","pending":true,"fw":"1.2.0","port":8080}`
	replyB = `{"v":1,"dev":"112233445566","pending":true,"fw":"1.1.9","port":8081}`
)

func TestDiscoverZeroDevices(t *testing.T) {
	addr, probes := fakeBoardUDP(t) // 不回复
	devs, err := testUDP(addr, allowAny).Discover(context.Background())
	if err != nil || len(devs) != 0 {
		t.Fatalf("devs=%v err=%v，期望 0 块", devs, err)
	}
	if p := probes(); len(p) != 1 || p[0] != "SBPAIR?1" {
		t.Errorf("探测包 = %v，期望 [SBPAIR?1]", p)
	}
}

func TestDiscoverOneDevice(t *testing.T) {
	addr, _ := fakeBoardUDP(t, replyA)
	devs, err := testUDP(addr, allowAny).Discover(context.Background())
	if err != nil || len(devs) != 1 {
		t.Fatalf("devs=%v err=%v", devs, err)
	}
	d := devs[0]
	if d.Dev != "aabbccddeeff" || d.FW != "1.2.0" || d.Port != 8080 || d.IP != "127.0.0.1" || d.Short() != "eeff" {
		t.Errorf("设备 = %+v short=%s", d, d.Short())
	}
}

func TestDiscoverManyFiltersJunk(t *testing.T) {
	addr, _ := fakeBoardUDP(t,
		replyB, replyA, replyA, // 重复的只算一块
		`{"v":1,"dev":"deadbeef0001","pending":false,"fw":"1","port":8080}`, // 非待配对
		`{"v":2,"dev":"deadbeef0002","pending":true,"fw":"1","port":8080}`,  // 版本不认识
		`{"v":1,"dev":"zz","pending":true,"fw":"1","port":8080}`,            // dev 不合法
		`{"v":1,"dev":"deadbeef0003","pending":true,"fw":"1","port":0}`,     // 端口不合法
		`not json`,
	)
	devs, err := testUDP(addr, allowAny).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 || devs[0].Dev != "112233445566" || devs[1].Dev != "aabbccddeeff" {
		t.Fatalf("devs = %+v，期望 2 块（按 dev 排序）", devs)
	}
}

// 默认过滤器只收私有地址：回环来源的回复被忽略。
func TestDiscoverIgnoresNonPrivateReplies(t *testing.T) {
	addr, _ := fakeBoardUDP(t, replyA)
	devs, err := testUDP(addr, isPrivateLAN).Discover(context.Background())
	if err != nil || len(devs) != 0 {
		t.Fatalf("非私有来源应被忽略：devs=%v err=%v", devs, err)
	}
	if d := newUDPDiscoverer(); d.addr != "255.255.255.255:5141" || d.wait != 2*time.Second || d.accept == nil {
		t.Errorf("默认参数不对：%+v", d)
	}
}

func TestIsPrivateLAN(t *testing.T) {
	for ip, want := range map[string]bool{
		"10.0.0.5": true, "10.255.255.254": true, "172.16.0.1": true, "172.31.255.1": true,
		"192.168.1.20": true,
		"172.15.0.1":   false, "172.32.0.1": false, "192.169.0.1": false, "11.0.0.1": false,
		"8.8.8.8": false, "127.0.0.1": false, "169.254.1.1": false, "0.0.0.0": false,
		"::1": false, "fe80::1": false, "2001:db8::1": false,
	} {
		if got := isPrivateLAN(net.ParseIP(ip)); got != want {
			t.Errorf("isPrivateLAN(%s) = %v，期望 %v", ip, got, want)
		}
	}
}

func TestParseReplyUsesSourceIPNotPayload(t *testing.T) {
	d, ok := parseReply([]byte(`{"v":1,"dev":"AABBCCDDEEFF","pending":true,"fw":"1.0","port":8080,"ip":"8.8.8.8"}`),
		net.ParseIP("192.168.1.9"), isPrivateLAN)
	if !ok || d.IP != "192.168.1.9" || d.Dev != "aabbccddeeff" {
		t.Fatalf("%+v ok=%v", d, ok)
	}
}

// ---- 读码 ----

func fakeBoardHTTP(t *testing.T, h http.HandlerFunc) foundDevice {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	host, p, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	port, _ := strconv.Atoi(p)
	return foundDevice{Dev: "aabbccddeeff", IP: host, Port: port}
}

func testReader() *httpCodeReader {
	r := newHTTPCodeReader()
	r.accept = allowAny
	r.timeout = 500 * time.Millisecond
	return r
}

func TestReadCodeOK(t *testing.T) {
	var gotPath string
	d := fakeBoardHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"dev":"AABBCCDDEEFF","code":"123456","expires_in":420}`))
	})
	code, err := testReader().ReadCode(context.Background(), d)
	if err != nil || code != "123456" || gotPath != "/pair" {
		t.Fatalf("code=%q err=%v path=%s", code, err, gotPath)
	}
}

func TestReadCodeNotPending404(t *testing.T) {
	d := fakeBoardHTTP(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	if _, err := testReader().ReadCode(context.Background(), d); !errors.Is(err, errNotPending) {
		t.Fatalf("err = %v，期望 errNotPending", err)
	}
}

func TestReadCodeExpired(t *testing.T) {
	d := fakeBoardHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"dev":"aabbccddeeff","code":"123456","expires_in":0}`))
	})
	_, err := testReader().ReadCode(context.Background(), d)
	if !errors.Is(err, errCodeExpired) {
		t.Fatalf("err = %v，期望 errCodeExpired", err)
	}
	if strings.Contains(err.Error(), "123456") {
		t.Error("错误文本不该带码")
	}
}

func TestReadCodeRejectsBadResponses(t *testing.T) {
	cases := map[string]string{
		"dev 不一致":  `{"dev":"000000000000","code":"123456","expires_in":10}`,
		"码格式不对":    `{"dev":"aabbccddeeff","code":"12ab56","expires_in":10}`,
		"不是 JSON":  `<html>`,
		"缺 dev":    `{"code":"123456","expires_in":10}`,
		"码太长":      `{"dev":"aabbccddeeff","code":"1234567","expires_in":10}`,
		"expires负": `{"dev":"aabbccddeeff","code":"123456","expires_in":-5}`,
	}
	for name, body := range cases {
		d := fakeBoardHTTP(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
		code, err := testReader().ReadCode(context.Background(), d)
		if err == nil || code != "" {
			t.Errorf("%s：code=%q err=%v，应当拒绝", name, code, err)
		}
	}
}

func TestReadCodeServerErrorTimeoutAndRedirect(t *testing.T) {
	d := fakeBoardHTTP(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	if _, err := testReader().ReadCode(context.Background(), d); err == nil || errors.Is(err, errNotPending) {
		t.Errorf("500 应是普通错误，得到 %v", err)
	}

	slow := fakeBoardHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	start := time.Now()
	if _, err := testReader().ReadCode(context.Background(), slow); err == nil {
		t.Error("超时应报错")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("超时没生效，耗时 %s", time.Since(start))
	}

	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(`{"dev":"aabbccddeeff","code":"123456","expires_in":10}`))
	}))
	defer target.Close()
	redir := fakeBoardHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/pair", http.StatusFound)
	})
	if _, err := testReader().ReadCode(context.Background(), redir); err == nil || hit {
		t.Errorf("不应跟随重定向：err=%v hit=%v", err, hit)
	}
}

func TestReadCodeRefusesNonPrivateAddress(t *testing.T) {
	d := fakeBoardHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("非私有地址不该发请求")
	})
	// 默认过滤器：回环（假板子所在地址）和公网都拒绝，且不发请求。
	for _, ip := range []string{d.IP, "8.8.8.8", "169.254.1.1"} {
		x := d
		x.IP = ip
		if _, err := newHTTPCodeReader().ReadCode(context.Background(), x); err == nil {
			t.Errorf("%s 应被拒绝", ip)
		}
	}
}

// ---- CLI：pair --auto ----

type fakeDisc struct {
	devs []foundDevice
	err  error
}

func (f fakeDisc) Discover(context.Context) ([]foundDevice, error) { return f.devs, f.err }

type fakeReader struct {
	mu    sync.Mutex
	codes map[string]string
	errs  map[string]error
	reads []string
}

func (f *fakeReader) ReadCode(_ context.Context, d foundDevice) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, d.Dev)
	if err := f.errs[d.Dev]; err != nil {
		return "", err
	}
	return f.codes[d.Dev], nil
}

var (
	devA = foundDevice{Dev: "aabbccddeeff", IP: "192.168.1.10", Port: 8080, FW: "1.2.0"}
	devB = foundDevice{Dev: "112233445566", IP: "192.168.1.11", Port: 8080, FW: "1.1.9"}
)

func newAutoEnv(t *testing.T) (*fakeStickbox, uiOptions) {
	t.Helper()
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	f := newFakeStickbox(t)
	return f, uiOptions{server: f.URL}
}

func TestPairAutoOneDeviceConfirmed(t *testing.T) {
	f, opts := newAutoEnv(t)
	rd := &fakeReader{codes: map[string]string{devA.Dev: "123456"}}
	var out bytes.Buffer
	err := pairAuto(context.Background(), opts, false, fakeDisc{devs: []foundDevice{devA}}, rd, strings.NewReader("y\n"), &out)
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	if c, _ := loadConfig(); c.Token != fakeAgentToken {
		t.Error("应与手输 pair 一样落盘令牌")
	}
	if f.calls() != 1 || len(rd.reads) != 1 {
		t.Errorf("pair 调用 %d 次，读码 %d 次", f.calls(), len(rd.reads))
	}
	s := out.String()
	if strings.Contains(s, "123456") || strings.Contains(s, fakeAgentToken) || !strings.Contains(s, "配对成功") || !strings.Contains(s, "eeff") {
		t.Errorf("输出不对（不得含码/令牌）：%s", s)
	}
}

func TestPairAutoOneDeviceDeclined(t *testing.T) {
	f, opts := newAutoEnv(t)
	rd := &fakeReader{codes: map[string]string{devA.Dev: "123456"}}
	for _, in := range []string{"n\n", "\n", ""} {
		err := pairAuto(context.Background(), opts, false, fakeDisc{devs: []foundDevice{devA}}, rd, strings.NewReader(in), &bytes.Buffer{})
		if err == nil {
			t.Errorf("输入 %q 应取消", in)
		}
	}
	if len(rd.reads) != 0 || f.calls() != 0 {
		t.Error("取消时不能读码或配对")
	}
}

func TestPairAutoYesSkipsConfirm(t *testing.T) {
	_, opts := newAutoEnv(t)
	rd := &fakeReader{codes: map[string]string{devA.Dev: "123456"}}
	if err := pairAuto(context.Background(), opts, true, fakeDisc{devs: []foundDevice{devA}}, rd, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestPairAutoMultipleChoose(t *testing.T) {
	f, opts := newAutoEnv(t)
	rd := &fakeReader{codes: map[string]string{devA.Dev: "000000", devB.Dev: "123456"}}
	var out bytes.Buffer
	// 排序后 devs 顺序由 discoverer 决定；这里 B 在第 2 位。
	err := pairAuto(context.Background(), opts, false, fakeDisc{devs: []foundDevice{devA, devB}}, rd, strings.NewReader("2\n"), &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(rd.reads) != 1 || rd.reads[0] != devB.Dev || f.calls() != 1 {
		t.Errorf("应只读选中的那块：reads=%v", rd.reads)
	}
	if !strings.Contains(out.String(), "…eeff") || !strings.Contains(out.String(), "…5566") {
		t.Errorf("应列出两块的后 4 位：%s", out.String())
	}
	// 非法序号取消；--yes 在多块时拒绝替用户选
	rd2 := &fakeReader{}
	if err := pairAuto(context.Background(), opts, false, fakeDisc{devs: []foundDevice{devA, devB}}, rd2, strings.NewReader("9\n"), &bytes.Buffer{}); err == nil || len(rd2.reads) != 0 {
		t.Errorf("非法序号应取消：%v", err)
	}
	if err := pairAuto(context.Background(), opts, true, fakeDisc{devs: []foundDevice{devA, devB}}, rd2, strings.NewReader(""), &bytes.Buffer{}); err == nil || len(rd2.reads) != 0 {
		t.Errorf("--yes 多块应拒绝：%v", err)
	}
}

func TestPairAutoNothingFoundAndErrors(t *testing.T) {
	_, opts := newAutoEnv(t)
	if err := pairAuto(context.Background(), opts, true, fakeDisc{}, &fakeReader{}, strings.NewReader(""), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "没找到") {
		t.Errorf("0 块应报没找到：%v", err)
	}
	for name, e := range map[string]error{"404": errNotPending, "过期": errCodeExpired} {
		rd := &fakeReader{errs: map[string]error{devA.Dev: e}}
		err := pairAuto(context.Background(), opts, true, fakeDisc{devs: []foundDevice{devA}}, rd, strings.NewReader(""), &bytes.Buffer{})
		if !errors.Is(err, e) {
			t.Errorf("%s：err = %v", name, err)
		}
	}
	if c, _ := loadConfig(); c.Token != "" {
		t.Error("失败不应保存令牌")
	}
}

func TestRunDiscoverPrintsList(t *testing.T) {
	var out bytes.Buffer
	if err := runDiscover(context.Background(), fakeDisc{devs: []foundDevice{devA, devB}}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "找到 2 块") || !strings.Contains(s, "192.168.1.10") || !strings.Contains(s, "1.1.9") {
		t.Errorf("输出 = %s", s)
	}
	out.Reset()
	_ = runDiscover(context.Background(), fakeDisc{}, &out)
	if !strings.Contains(out.String(), "没找到") {
		t.Errorf("0 块输出 = %s", out.String())
	}
}

// ---- 网页：/api/discover、/api/pair-device ----

func TestUIDiscoverAndPairDevice(t *testing.T) {
	e := newAPIEnv(t)
	rd := &fakeReader{codes: map[string]string{devA.Dev: "123456"}}
	e.s.disc = fakeDisc{devs: []foundDevice{devA, devB}}
	e.s.codes = rd

	// 没搜索过就配对：不认
	if code, _ := e.post(t, "/api/pair-device", `{"dev":"aabbccddeeff"}`); code != 404 {
		t.Fatalf("未搜索就配对 = %d，期望 404", code)
	}
	code, body := e.get(t, "/api/discover")
	if code != 200 || !strings.Contains(body, `"short":"eeff"`) || !strings.Contains(body, "1.2.0") || !strings.Contains(body, "192.168.1.10") {
		t.Fatalf("discover = %d %s", code, body)
	}
	if strings.Contains(body, "123456") {
		t.Error("发现结果不含码")
	}
	// 页面传的 ip/port 一概不看；服务端只认自己记下的
	code, body = e.post(t, "/api/pair-device", `{"dev":"AABBCCDDEEFF","ip":"8.8.8.8","port":1}`)
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("pair-device = %d %s", code, body)
	}
	all := body + e.logs.String()
	if strings.Contains(all, "123456") || strings.Contains(all, fakeAgentToken) {
		t.Fatalf("响应/日志里出现了码或令牌：%s", all)
	}
	if c, _ := loadConfig(); c.Token != fakeAgentToken {
		t.Error("应落盘令牌")
	}
	select {
	case <-e.s.paired:
	default:
		t.Error("配对成功应触发退出计时")
	}
}

func TestUIPairDeviceFailures(t *testing.T) {
	e := newAPIEnv(t)
	rd := &fakeReader{codes: map[string]string{}, errs: map[string]error{devA.Dev: errNotPending, devB.Dev: errCodeExpired}}
	e.s.disc = fakeDisc{devs: []foundDevice{devA, devB}}
	e.s.codes = rd
	e.get(t, "/api/discover")

	if code, body := e.post(t, "/api/pair-device", `{"dev":"aabbccddeeff"}`); code != 409 || !strings.Contains(body, "not_pending") {
		t.Errorf("读码 404 = %d %s", code, body)
	}
	if code, body := e.post(t, "/api/pair-device", `{"dev":"112233445566"}`); code != 409 || !strings.Contains(body, "code_expired") {
		t.Errorf("码过期 = %d %s", code, body)
	}
	// 读到了码，但服务端拒绝（码错/过期）：同手输的提示，并计入限速
	rd.codes[devA.Dev] = "000000"
	delete(rd.errs, devA.Dev)
	code, body := e.post(t, "/api/pair-device", `{"dev":"aabbccddeeff"}`)
	if code != 400 || !strings.Contains(body, "每 10 分钟换新") {
		t.Errorf("服务端拒绝 = %d %s", code, body)
	}
	e.s.failMu.Lock()
	n := len(e.s.failures)
	e.s.failMu.Unlock()
	if n != 1 {
		t.Errorf("服务端拒绝应计入限速，计了 %d", n)
	}
	if strings.Contains(body+e.logs.String(), "000000") {
		t.Error("码不得进响应或日志")
	}
	if c, _ := loadConfig(); c.Token != "" {
		t.Error("失败不应保存令牌")
	}
}

func TestUIDiscoverErrorAndAuth(t *testing.T) {
	e := newAPIEnv(t)
	e.s.disc = fakeDisc{err: errors.New("udp boom")}
	if code, body := e.get(t, "/api/discover"); code != 502 || !strings.Contains(body, "discover") {
		t.Errorf("= %d %s", code, body)
	}
	// 新端点同样要 token / Origin
	if code, _, _ := do(t, e.ts, "GET", "/api/discover", nil, ""); code != 401 {
		t.Errorf("discover 无 token = %d", code)
	}
	if code, _, _ := do(t, e.ts, "POST", "/api/pair-device", map[string]string{
		"X-Stickbox-Token": e.s.token, "Origin": "http://evil.example.com", "Content-Type": "application/json",
	}, `{"dev":"aabbccddeeff"}`); code != 403 {
		t.Errorf("pair-device 跨站 = %d", code)
	}
}
