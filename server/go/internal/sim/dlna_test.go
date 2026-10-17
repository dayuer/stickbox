package sim

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// 三星 Room C 的 SOAP 解析器不认数字实体（&#34; / &#39;），
// 所以只转义 & < >，引号原样保留（与 saxutils.escape 一致）。
func TestXmlEscapeOnlyAmpLtGt(t *testing.T) {
	in := `a&b<c>d "q" 'p'`
	want := `a&amp;b&lt;c&gt;d "q" 'p'`
	if got := xmlEscape(in); got != want {
		t.Errorf("xmlEscape(%q) = %q，期望 %q", in, got, want)
	}
	if got := xmlEscape(in); strings.Contains(got, "&#") {
		t.Errorf("输出里不能出现数字实体：%q", got)
	}
}

// SetAVTransportURI 的 SOAP 请求体必须和 T17 的 dlna_push.py 逐字节相同。
// golden 由 testdata/gen_soap_golden.py 调用 dlna_push.py 生成。
func TestSoapSetAVTransportURIMatchesPythonGolden(t *testing.T) {
	gold, err := os.ReadFile("testdata/soap_setavtransporturi.xml")
	if err != nil {
		t.Fatal(err)
	}
	const url = "http://192.168.3.237:8091/live.avi"
	got := soapEnvelope("SetAVTransportURI", [][2]string{
		{"InstanceID", "0"},
		{"CurrentURI", xmlEscape(url)},
		{"CurrentURIMetaData", didl(url, "StickBox", "video/avi")},
	})
	if got != string(gold) {
		i := 0
		for i < len(got) && i < len(gold) && got[i] == gold[i] {
			i++
		}
		t.Fatalf("SOAP 与 dlna_push.py 不一致（第 %d 字节）：\n got: %.120q\nwant: %.120q",
			i, got[max(0, i-40):min(len(got), i+80)], string(gold)[max(0, i-40):min(len(gold), i+80)])
	}
	if strings.Contains(got, "&#") {
		t.Errorf("SOAP 里不能出现数字实体")
	}
}

// 三星 AVTransport:1 的 GetTransportInfo 输出字段是
// CurrentTransportState（不是 TransportState），别写错。
func TestTransportStateParsesAVTransport1Response(t *testing.T) {
	var action string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action = r.Header.Get("SOAPACTION")
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>`+
			`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" `+
			`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`+
			`<u:GetTransportInfoResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">`+
			`<CurrentTransportState>PLAYING</CurrentTransportState>`+
			`<CurrentTransportStatus>OK</CurrentTransportStatus>`+
			`<CurrentSpeed>1</CurrentSpeed>`+
			`</u:GetTransportInfoResponse></s:Body></s:Envelope>`)
	}))
	defer srv.Close()

	state, err := transportState(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if state != "PLAYING" {
		t.Errorf("state = %q", state)
	}
	if action != `"urn:schemas-upnp-org:service:AVTransport:1#GetTransportInfo"` {
		t.Errorf("SOAPACTION = %q", action)
	}
}

type fakeTransport struct {
	t       *testing.T
	states  []string // 依次返回给 GetTransportInfo，最后一个重复使用
	mu      sync.Mutex
	calls   []transportCall
	getCall int
}

type transportCall struct {
	action string
	at     time.Time
	body   string
}

func newFakeTransport(t *testing.T, states ...string) (*fakeTransport, *httptest.Server) {
	f := &fakeTransport{t: t, states: states}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		action := strings.Trim(r.Header.Get("SOAPACTION"), `"`)
		if i := strings.LastIndex(action, "#"); i >= 0 {
			action = action[i+1:]
		}
		f.mu.Lock()
		f.calls = append(f.calls, transportCall{action: action, at: time.Now(), body: string(raw)})
		state := ""
		if action == "GetTransportInfo" {
			i := min(f.getCall, len(f.states)-1)
			state = f.states[i]
			f.getCall++
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		if action == "GetTransportInfo" {
			io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+
				`<u:GetTransportInfoResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">`+
				`<CurrentTransportState>`+state+`</CurrentTransportState></u:GetTransportInfoResponse>`+
				`</s:Body></s:Envelope>`)
			return
		}
		io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body/></s:Envelope>`)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeTransport) snapshot() []transportCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transportCall(nil), f.calls...)
}

// 固定返回一个已经拉了足够久的连接。
type fixedProbe time.Duration

func (p fixedProbe) ActiveFor() time.Duration { return time.Duration(p) }
func (p fixedProbe) Active() bool             { return p > 0 }

// growProbe 前 zero 次返回 0，之后返回 age：模拟电视稍后才连上
// /live.avi 并持续拉流。
type growProbe struct {
	zero int
	age  time.Duration
	n    int
}

func (p *growProbe) ActiveFor() time.Duration {
	p.n++
	if p.n <= p.zero {
		return 0
	}
	return p.age
}

func (p *growProbe) Active() bool { return p.n > p.zero }

// flipProbe 在 push 调用之后才开始有连接：模拟推送失败时电视其实已经
// 自己连上来拉流了。
type flipProbe struct{ on bool }

func (p *flipProbe) ActiveFor() time.Duration {
	if p.on {
		return time.Second
	}
	return 0
}

func (p *flipProbe) Active() bool { return p.on }

// 推送顺序 Stop → (等 STOPPED) → SetAVTransportURI → Play → GetTransportInfo；
// SetAVTransportURI 必须在 Stop 之后至少 200ms（三星切状态要 200–500ms）。
func TestPushToControlWaitsAfterStopAndConfirmsPlaying(t *testing.T) {
	fake, srv := newFakeTransport(t, "PLAYING")
	const url = "http://192.168.3.237:8091/live.avi"
	p := pushParams{
		stopSettle:   stopSettle,
		poll:         10 * time.Millisecond,
		playTimeout:  time.Second,
		minStreamAge: time.Millisecond,
	}
	if err := pushToControl(context.Background(), srv.URL, url, fixedProbe(time.Second), p); err != nil {
		t.Fatal(err)
	}

	calls := fake.snapshot()
	var order []string
	for _, c := range calls {
		order = append(order, c.action)
	}
	want := []string{"Stop", "SetAVTransportURI", "Play", "GetTransportInfo"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("调用顺序 = %v，期望 %v", order, want)
	}
	if d := calls[1].at.Sub(calls[0].at); d < 200*time.Millisecond {
		t.Errorf("Stop 到 SetAVTransportURI 只等了 %s，三星需要 200–500ms", d)
	}
	if strings.Contains(calls[1].body, "&#") {
		t.Errorf("SetAVTransportURI 里不能出现数字实体")
	}
	if !strings.Contains(calls[1].body, `xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"`) {
		t.Errorf("SOAP 结构不对：%s", calls[1].body)
	}
}

// 三星 Play 之后先回 TRANSITIONING（1–2.5s），不能当成失败去 Stop 重推。
func TestPushToControlPollsThroughTransitioning(t *testing.T) {
	fake, srv := newFakeTransport(t, "TRANSITIONING", "TRANSITIONING", "TRANSITIONING", "PLAYING")
	p := pushParams{
		stopSettle:   time.Millisecond,
		poll:         5 * time.Millisecond,
		playTimeout:  time.Second,
		minStreamAge: time.Millisecond,
	}
	if err := pushToControl(context.Background(), srv.URL, "http://x/live.avi", fixedProbe(time.Second), p); err != nil {
		t.Fatal(err)
	}
	calls := fake.snapshot()
	sets, gets := 0, 0
	for _, c := range calls {
		switch c.action {
		case "SetAVTransportURI":
			sets++
		case "GetTransportInfo":
			gets++
		}
	}
	if sets != 1 {
		t.Errorf("SetAVTransportURI 调了 %d 次，TRANSITIONING 期间不应重推", sets)
	}
	if gets != 4 {
		t.Errorf("GetTransportInfo 调了 %d 次，期望 4", gets)
	}
}

// 状态已经是 PLAYING，但电视还没开始拉流，也不能算成功。
func TestPushToControlWaitsForLiveStreamConnection(t *testing.T) {
	_, srv := newFakeTransport(t, "PLAYING")
	probe := &growProbe{zero: 3, age: 10 * time.Second}
	p := pushParams{
		stopSettle:   time.Millisecond,
		poll:         5 * time.Millisecond,
		playTimeout:  time.Second,
		minStreamAge: time.Second,
	}
	if err := pushToControl(context.Background(), srv.URL, "http://x/live.avi", probe, p); err != nil {
		t.Fatal(err)
	}
	if probe.n < 4 {
		t.Errorf("拉流连接出现前就判成功了（探测 %d 次）", probe.n)
	}
}

func TestPushToControlFailsWhenStopped(t *testing.T) {
	fake, srv := newFakeTransport(t, "STOPPED")
	p := pushParams{
		stopSettle:   time.Millisecond,
		poll:         5 * time.Millisecond,
		playTimeout:  time.Second,
		minStreamAge: time.Millisecond,
	}
	err := pushToControl(context.Background(), srv.URL, "http://x/live.avi", fixedProbe(time.Second), p)
	if err == nil || !strings.Contains(err.Error(), "STOPPED") {
		t.Fatalf("err = %v，期望 STOPPED 失败", err)
	}
	gets := 0
	for _, c := range fake.snapshot() {
		if c.action == "GetTransportInfo" {
			gets++
		}
	}
	if gets != 1 {
		t.Errorf("STOPPED 应立即失败，GetTransportInfo 调了 %d 次", gets)
	}
}

func TestPushToControlTimesOutWithoutPlaying(t *testing.T) {
	_, srv := newFakeTransport(t, "TRANSITIONING")
	p := pushParams{
		stopSettle:   time.Millisecond,
		poll:         5 * time.Millisecond,
		playTimeout:  40 * time.Millisecond,
		minStreamAge: time.Millisecond,
	}
	if err := pushToControl(context.Background(), srv.URL, "http://x/live.avi", fixedProbe(time.Second), p); err == nil {
		t.Fatal("一直 TRANSITIONING 应超时失败")
	}
}

func TestPushWithRetrySucceedsAfterBackoff(t *testing.T) {
	calls := 0
	push := func(context.Context, string, string, streamProbe) error {
		calls++
		if calls < 3 {
			return errors.New("状态不是 PLAYING")
		}
		return nil
	}
	var logs int
	u, pushed, err := pushWithRetry(context.Background(), "192.168.3.18", ":8091",
		func(string) (string, error) { return "192.168.3.237", nil },
		push, fixedProbe(0), []time.Duration{time.Millisecond, time.Millisecond},
		func(string, ...any) { logs++ })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("推送次数 = %d，期望 3", calls)
	}
	if !pushed {
		t.Error("成功后 pushed 应为 true")
	}
	if logs != 2 {
		t.Errorf("重试日志 = %d，期望 2", logs)
	}
	if u != "http://192.168.3.237:8091/live.avi" {
		t.Errorf("URL = %q", u)
	}
}

func TestPushWithRetryGivesUpAfterDelays(t *testing.T) {
	calls := 0
	push := func(context.Context, string, string, streamProbe) error {
		calls++
		return errors.New("402 Invalid Args")
	}
	_, _, err := pushWithRetry(context.Background(), "192.168.3.18", ":8091",
		func(string) (string, error) { return "192.168.3.237", nil },
		push, fixedProbe(0), []time.Duration{time.Millisecond, time.Millisecond},
		func(string, ...any) {})
	if err == nil {
		t.Fatal("一直失败时应返回错误")
	}
	if calls != 3 {
		t.Errorf("推送次数 = %d，期望 3（1 次 + 2 次重试）", calls)
	}
}

func TestPushWithRetryStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	push := func(context.Context, string, string, streamProbe) error {
		calls++
		cancel()
		return errors.New("失败")
	}
	_, _, err := pushWithRetry(ctx, "192.168.3.18", ":8091",
		func(string) (string, error) { return "192.168.3.237", nil },
		push, fixedProbe(0), []time.Duration{time.Hour},
		func(string, ...any) {})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v，期望 context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("取消后不应再推送，实际 %d 次", calls)
	}
}

// 电视已经在拉流时，重推会打断播放：直接跳过，不算失败。
func TestPushWithRetrySkipsWhenTVAlreadyPulling(t *testing.T) {
	calls := 0
	probe := &flipProbe{}
	push := func(context.Context, string, string, streamProbe) error {
		calls++
		probe.on = true // 推送失败的当口，电视已经连上来拉流了
		return errors.New("状态是 TRANSITIONING")
	}
	u, pushed, err := pushWithRetry(context.Background(), "192.168.3.18", ":8091",
		func(string) (string, error) { return "192.168.3.237", nil },
		push, probe, []time.Duration{time.Hour},
		func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if pushed {
		t.Error("已有拉流连接时 pushed 应为 false")
	}
	if calls != 1 {
		t.Errorf("已有拉流连接时不应重推，实际 %d 次", calls)
	}
	if u != "http://192.168.3.237:8091/live.avi" {
		t.Errorf("URL = %q", u)
	}
}
