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

// 推送顺序 Stop → (等 STOPPED) → SetAVTransportURI → Play → GetTransportInfo；
// SetAVTransportURI 必须在 Stop 之后至少 200ms（三星切状态要 200–500ms）。
func TestPushToControlWaitsAfterStopAndConfirmsPlaying(t *testing.T) {
	type call struct {
		action string
		at     time.Time
		body   string
	}
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		action := strings.Trim(r.Header.Get("SOAPACTION"), `"`)
		if i := strings.LastIndex(action, "#"); i >= 0 {
			action = action[i+1:]
		}
		mu.Lock()
		calls = append(calls, call{action: action, at: time.Now(), body: string(raw)})
		mu.Unlock()

		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		if action == "GetTransportInfo" {
			io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+
				`<u:GetTransportInfoResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">`+
				`<CurrentTransportState>PLAYING</CurrentTransportState></u:GetTransportInfoResponse>`+
				`</s:Body></s:Envelope>`)
			return
		}
		io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body/></s:Envelope>`)
	}))
	defer srv.Close()

	const url = "http://192.168.3.237:8091/live.avi"
	if err := pushToControl(context.Background(), srv.URL, url); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
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

func TestPushWithRetrySucceedsAfterBackoff(t *testing.T) {
	calls := 0
	push := func(context.Context, string, string) error {
		calls++
		if calls < 3 {
			return errors.New("状态不是 PLAYING")
		}
		return nil
	}
	var logs int
	u, err := pushWithRetry(context.Background(), "192.168.3.18", ":8091",
		func(string) (string, error) { return "192.168.3.237", nil },
		push, []time.Duration{time.Millisecond, time.Millisecond},
		func(string, ...any) { logs++ })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("推送次数 = %d，期望 3", calls)
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
	push := func(context.Context, string, string) error {
		calls++
		return errors.New("402 Invalid Args")
	}
	_, err := pushWithRetry(context.Background(), "192.168.3.18", ":8091",
		func(string) (string, error) { return "192.168.3.237", nil },
		push, []time.Duration{time.Millisecond, time.Millisecond},
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
	push := func(context.Context, string, string) error {
		calls++
		cancel()
		return errors.New("失败")
	}
	_, err := pushWithRetry(ctx, "192.168.3.18", ":8091",
		func(string) (string, error) { return "192.168.3.237", nil },
		push, []time.Duration{time.Hour},
		func(string, ...any) {})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v，期望 context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("取消后不应再推送，实际 %d 次", calls)
	}
}
