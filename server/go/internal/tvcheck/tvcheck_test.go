package tvcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTV 是一个最小的 DLNA 渲染器：设备描述 + AVTransport SOAP + 拉流客户端。
type fakeTV struct {
	mu sync.Mutex

	setURIStatus int
	playStatus   int
	sink         string
	states       []string
	pollIndex    int
	streamURL    string

	dropAfter  time.Duration
	pullRounds int

	done chan struct{}
}

func newFakeTV(t *testing.T) (*fakeTV, *httptest.Server) {
	t.Helper()
	f := &fakeTV{
		setURIStatus: http.StatusOK,
		playStatus:   http.StatusOK,
		sink:         "http-get:*:video/avi:*,http-get:*:video/mp4:*",
		states:       []string{"TRANSITIONING", "TRANSITIONING", "PLAYING"},
		done:         make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/desc.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0"><device>
  <friendlyName>Fake Samsung</friendlyName>
  <manufacturer>Samsung</manufacturer>
  <modelName>Room C</modelName>
  <serviceList><service>
    <serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
    <controlURL>/ctl</controlURL>
  </service></serviceList>
</device></root>`)
	})
	mux.HandleFunc("/ctl", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		action := strings.Trim(r.Header.Get("SOAPACTION"), `"`)
		if i := strings.LastIndex(action, "#"); i >= 0 {
			action = action[i+1:]
		}
		f.mu.Lock()
		switch action {
		case "GetProtocolInfo":
			sink := f.sink
			f.mu.Unlock()
			fmt.Fprintf(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+
				`<u:GetProtocolInfoResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">`+
				`<Source></Source><Sink>%s</Sink></u:GetProtocolInfoResponse></s:Body></s:Envelope>`, sink)
			return
		case "SetAVTransportURI":
			uri := currentURI(string(raw))
			f.streamURL = uri
			st := f.setURIStatus
			f.mu.Unlock()
			w.WriteHeader(st)
			fmt.Fprint(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body/></s:Envelope>`)
			return
		case "Play":
			st := f.playStatus
			url := f.streamURL
			f.mu.Unlock()
			w.WriteHeader(st)
			fmt.Fprint(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body/></s:Envelope>`)
			if url != "" {
				go f.pull(url)
			}
			return
		case "GetTransportInfo":
			state := "PLAYING"
			if len(f.states) > 0 {
				i := f.pollIndex
				if i >= len(f.states) {
					i = len(f.states) - 1
				}
				state = f.states[i]
				if f.pollIndex < len(f.states) {
					f.pollIndex++
				}
			}
			f.mu.Unlock()
			fmt.Fprintf(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+
				`<u:GetTransportInfoResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">`+
				`<CurrentTransportState>%s</CurrentTransportState></u:GetTransportInfoResponse>`+
				`</s:Body></s:Envelope>`, state)
			return
		default:
			f.mu.Unlock()
			fmt.Fprint(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body/></s:Envelope>`)
			return
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		close(f.done)
		srv.Close()
	})
	return f, srv
}

var uriRe = regexp.MustCompile(`<CurrentURI>(.*?)</CurrentURI>`)

func currentURI(soap string) string {
	m := uriRe.FindStringSubmatch(soap)
	if len(m) < 2 {
		return ""
	}
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(m[1], "&amp;", "&"), "&lt;", "<"), "&gt;", ">")
}

// pull 模拟电视：先 HEAD 探测，再 GET 拉流。dropAfter 非 0 时断开并重连。
func (f *fakeTV) pull(url string) {
	client := &http.Client{Timeout: 5 * time.Second}
	head, err := http.NewRequest(http.MethodHead, url, nil)
	if err == nil {
		head.Header.Set("User-Agent", "FakeTV/1.0")
		head.Header.Set("getcontentFeatures.dlna.org", "1")
		if resp, err := client.Do(head); err == nil {
			resp.Body.Close()
		}
	}
	rounds := f.pullRounds
	if rounds == 0 {
		rounds = 1
	}
	for i := 0; i < rounds; i++ {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return
		}
		req.Header.Set("User-Agent", "FakeTV/1.0")
		req.Header.Set("transferMode.dlna.org", "Streaming")
		if i == 0 {
			req.Header.Set("Range", "bytes=0-")
		}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
		if f.dropAfter > 0 && i+1 < rounds {
			time.Sleep(f.dropAfter)
		}
		resp.Body.Close()
		if f.dropAfter > 0 && i+1 < rounds {
			time.Sleep(300 * time.Millisecond)
		}
	}
}

func testOptions() Options {
	return Options{
		Duration:     500 * time.Millisecond,
		FPS:          10,
		Burst:        8,
		Listen:       "127.0.0.1:0",
		HostFor:      func(string) (string, error) { return "127.0.0.1", nil },
		PollInterval: 10 * time.Millisecond,
		PlayTimeout:  time.Second,
		StopSettle:   time.Millisecond,
	}
}

func TestRunSamsungLikeTV(t *testing.T) {
	f, srv := newFakeTV(t)
	rep, err := Run(context.Background(), Renderer{
		Name:         "Fake Samsung",
		Manufacturer: "Samsung",
		Model:        "Room C",
		IP:           "127.0.0.1",
		ControlURL:   srv.URL + "/ctl",
		Location:     srv.URL + "/desc.xml",
	}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Push.SetURIStatus != 200 || rep.Push.PlayStatus != 200 {
		t.Fatalf("SOAP 状态 = %+v", rep.Push)
	}
	if rep.Push.TimeToPlayingMS == nil {
		t.Fatal("没有记录到 PLAYING")
	}
	if rep.Push.TransitioningMS == nil {
		t.Error("TRANSITIONING 没有记录")
	}
	if rep.Stream.Connections == 0 {
		t.Fatal("没有收到电视的拉流请求")
	}
	if !rep.Stream.HTTP.SawHead || !rep.Stream.HTTP.SawRange {
		t.Errorf("请求头记录不完整：%+v", rep.Stream.HTTP)
	}
	if len(rep.Stream.HTTP.UserAgents) == 0 || rep.Stream.HTTP.UserAgents[0] != "FakeTV/1.0" {
		t.Errorf("User-Agent = %v", rep.Stream.HTTP.UserAgents)
	}
	if !hasWarning(rep, "transitioning", "info") {
		t.Errorf("缺少 TRANSITIONING 提示：%+v", rep.Warnings)
	}
	if rep.HasErrors() {
		t.Errorf("不应有 error：%+v", rep.Warnings)
	}
	raw, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("JSON 不能反序列化：%v", err)
	}
	md := rep.Markdown()
	for _, want := range []string{"Fake Samsung", "SetAVTransportURI", "进入 PLAYING", "User-Agent"} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown 缺少 %q：\n%s", want, md)
		}
	}
	_ = f
}

func TestRun402WarnsNumericEntities(t *testing.T) {
	f, srv := newFakeTV(t)
	f.setURIStatus = http.StatusPaymentRequired
	rep, err := Run(context.Background(), Renderer{Name: "Bad TV", IP: "127.0.0.1", ControlURL: srv.URL + "/ctl"}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(rep, "numeric_entity_402", "error") || !rep.HasErrors() {
		t.Fatalf("402 没有给出数字实体提示：%+v", rep.Warnings)
	}
}

func TestRunNoPlaying(t *testing.T) {
	f, srv := newFakeTV(t)
	f.states = []string{"TRANSITIONING", "TRANSITIONING", "TRANSITIONING"}
	opt := testOptions()
	opt.PlayTimeout = 80 * time.Millisecond
	rep, err := Run(context.Background(), Renderer{Name: "Slow TV", IP: "127.0.0.1", ControlURL: srv.URL + "/ctl"}, opt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Push.TimeToPlayingMS != nil || !hasWarning(rep, "no_playing", "error") {
		t.Fatalf("没有识别出未起播：%+v", rep)
	}
}

func TestRunMIMENotDeclared(t *testing.T) {
	f, srv := newFakeTV(t)
	f.sink = "http-get:*:video/mp4:*"
	rep, err := Run(context.Background(), Renderer{Name: "MP4 TV", IP: "127.0.0.1", ControlURL: srv.URL + "/ctl"}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(rep, "mime_not_declared", "warn") {
		t.Fatalf("没有给出 MIME 提示：%+v", rep.Warnings)
	}
}

func TestRunMeasuresReconnect(t *testing.T) {
	f, srv := newFakeTV(t)
	f.dropAfter = 120 * time.Millisecond
	f.pullRounds = 2
	opt := testOptions()
	opt.Duration = 900 * time.Millisecond
	rep, err := Run(context.Background(), Renderer{Name: "Flaky TV", IP: "127.0.0.1", ControlURL: srv.URL + "/ctl"}, opt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stream.Disconnects == 0 || rep.Stream.Connections < 2 {
		t.Fatalf("没有测到断开/重连：%+v", rep.Stream)
	}
	if len(rep.Stream.Reconnects) == 0 || rep.Stream.Reconnects[0].DowntimeMS < 0 {
		t.Fatalf("重连耗时没有记录：%+v", rep.Stream.Reconnects)
	}
	if !hasWarning(rep, "first_push_disconnect", "warn") {
		t.Errorf("首推断开提示缺失：%+v", rep.Warnings)
	}
}

func TestParseDescription(t *testing.T) {
	r, err := parseDescription("http://192.168.3.18:9197/desc.xml", []byte(`<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0"><device>
  <friendlyName>客厅电视</friendlyName>
  <manufacturer>Samsung</manufacturer>
  <modelName>UA60DU7000</modelName>
  <serviceList>
    <service><serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType><controlURL>/rc</controlURL></service>
    <service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType><controlURL>/upnp/control/AVTransport1</controlURL></service>
  </serviceList>
</device></root>`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "客厅电视" || r.IP != "192.168.3.18" ||
		r.ControlURL != "http://192.168.3.18:9197/upnp/control/AVTransport1" {
		t.Errorf("解析结果 = %+v", r)
	}

	r, err = parseDescription("http://192.168.3.18:9197/desc.xml", []byte(`<root><device>
  <friendlyName>No AVT</friendlyName></device></root>`))
	if err != nil {
		t.Fatal(err)
	}
	if r.ControlURL != "" {
		t.Errorf("没有 AVTransport 时 ControlURL = %q", r.ControlURL)
	}
}

func TestVideoMIMEs(t *testing.T) {
	got := videoMIMEs("http-get:*:video/avi:*,http-get:*:video/mp4:*,http-get:*:audio/mpeg:*,http-get:*:video/avi:*")
	want := []string{"video/avi", "video/mp4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("videoMIMEs = %v", got)
	}
}

func TestCardIsValidJPEG(t *testing.T) {
	c, err := NewCard("Fake TV")
	if err != nil {
		t.Fatal(err)
	}
	b := c.Current()
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		t.Fatalf("不是 JPEG：% x", b[:min(8, len(b))])
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("标准库解不开测试卡：%v", err)
	}
	if cfg.Width != 1920 || cfg.Height != 1080 {
		t.Errorf("尺寸 = %dx%d", cfg.Width, cfg.Height)
	}
}

func hasWarning(r *Report, id, severity string) bool {
	for _, w := range r.Warnings {
		if w.ID == id && w.Severity == severity {
			return true
		}
	}
	return false
}
