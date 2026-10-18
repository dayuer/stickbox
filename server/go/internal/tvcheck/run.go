package tvcheck

import (
	"context"
	"encoding/xml"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/sim"
)

const (
	ToolName = "stickbox-tvcheck"
	Version  = "0.1.0"
)

// Options 控制一次兼容性测试。零值会填默认值。
type Options struct {
	Duration     time.Duration
	FPS          int
	Burst        int
	Listen       string
	HostFor      func(tvIP string) (string, error)
	PollInterval time.Duration
	PlayTimeout  time.Duration
	StopSettle   time.Duration
	Now          func() time.Time
	Progress     func(string, ...any)
}

func (o *Options) fill() {
	if o.Duration <= 0 {
		o.Duration = 3 * time.Minute
	}
	if o.FPS <= 0 {
		o.FPS = 2
	}
	if o.Burst <= 0 {
		o.Burst = 8
	}
	if o.Listen == "" {
		o.Listen = ":0"
	}
	if o.HostFor == nil {
		o.HostFor = sim.LocalIPToward
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.PlayTimeout <= 0 {
		o.PlayTimeout = 15 * time.Second
	}
	if o.StopSettle <= 0 {
		o.StopSettle = 500 * time.Millisecond
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Progress == nil {
		o.Progress = func(string, ...any) {}
	}
}

type Report struct {
	Tool         string       `json:"tool"`
	Version      string       `json:"version"`
	GeneratedAt  string       `json:"generated_at"`
	Renderer     Renderer     `json:"renderer"`
	ProtocolInfo ProtocolInfo `json:"protocol_info"`
	Push         PushResult   `json:"push"`
	Stream       StreamResult `json:"stream"`
	Warnings     []Warning    `json:"warnings"`
}

type ProtocolInfo struct {
	Status    int      `json:"status"`
	Error     string   `json:"error,omitempty"`
	Source    string   `json:"source,omitempty"`
	Sink      string   `json:"sink,omitempty"`
	VideoMIME []string `json:"video_mime"`
}

type PushResult struct {
	StopStatus      int      `json:"stop_status"`
	SetURIStatus    int      `json:"set_uri_status"`
	PlayStatus      int      `json:"play_status"`
	StopError       string   `json:"stop_error,omitempty"`
	SetURIError     string   `json:"set_uri_error,omitempty"`
	PlayError       string   `json:"play_error,omitempty"`
	States          []string `json:"states"`
	TimeToPlayingMS *int64   `json:"time_to_playing_ms"`
	TransitioningMS *int64   `json:"transitioning_ms"`
}

type Reconnect struct {
	DowntimeMS int64 `json:"downtime_ms"`
}

type RequestObs struct {
	Method    string            `json:"method"`
	UserAgent string            `json:"user_agent,omitempty"`
	Range     string            `json:"range,omitempty"`
	AtMS      int64             `json:"at_ms"`
	Headers   map[string]string `json:"headers,omitempty"`
}

type HTTPObservations struct {
	Requests   []RequestObs `json:"requests"`
	UserAgents []string     `json:"user_agents,omitempty"`
	Methods    []string     `json:"methods,omitempty"`
	SawHead    bool         `json:"saw_head"`
	SawRange   bool         `json:"saw_range"`
}

type StreamResult struct {
	DurationSeconds        int              `json:"duration_s"`
	URL                    string           `json:"url"`
	Connections            int              `json:"connections"`
	Disconnects            int              `json:"disconnects"`
	Reconnects             []Reconnect      `json:"reconnects"`
	FirstDisconnectAfterMS *int64           `json:"first_disconnect_after_ms,omitempty"`
	HTTP                   HTTPObservations `json:"http"`
}

type Warning struct {
	ID       string `json:"id"`
	Severity string `json:"severity"` // info / warn / error
	Message  string `json:"message"`
	Hint     string `json:"hint,omitempty"`
}

// Run 对一台渲染器做完整测试。基础设施错误返回 (nil, err)；电视侧的失败
// 会写进报告 warnings，仍然返回报告。
func Run(ctx context.Context, r Renderer, opt Options) (*Report, error) {
	opt.fill()
	if r.ControlURL == "" {
		return nil, fmt.Errorf("%s 没有 AVTransport 控制地址，无法推送", r.Display())
	}
	started := opt.Now()
	rep := &Report{
		Tool:        ToolName,
		Version:     Version,
		GeneratedAt: started.Format(time.RFC3339),
		Renderer:    r,
	}

	// 1. 电视声明支持哪些 MIME。GetProtocolInfo 属于 ConnectionManager，
	// 不是 AVTransport；发错服务时三星会回 500。
	rep.ProtocolInfo = queryProtocolInfo(ctx, r.ConnectionManagerURL, r.ConnectionManagerService)

	// 2. 测试卡 + 推流服务。
	card, err := NewCard(r.Display())
	if err != nil {
		return nil, err
	}
	srv := sim.NewStreamServer(opt.Listen, opt.FPS, opt.Burst, card.Current)
	events := newEventLog(opt.Now)
	srv.SetEvents(events.hooks())
	if err := srv.Start(); err != nil {
		return nil, fmt.Errorf("启动推流服务：%w", err)
	}
	defer srv.Close()

	host, err := opt.HostFor(r.IP)
	if err != nil {
		return nil, fmt.Errorf("找不到电视可达的本机地址：%w", err)
	}
	streamURL, err := makeStreamURL(srv.Addr(), host)
	if err != nil {
		return nil, err
	}
	rep.Stream.URL = streamURL
	rep.Stream.DurationSeconds = int(opt.Duration.Seconds())

	cardCtx, stopCard := context.WithCancel(ctx)
	defer stopCard()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-cardCtx.Done():
				return
			case now := <-t.C:
				_ = card.Update(now)
			}
		}
	}()

	opt.Progress("推流地址：%s", streamURL)
	opt.Progress("开始推送（%s）", r.Display())

	// 3. Stop → SetURI → Play，记录真实返回码。
	rep.Push = pushStream(ctx, r.ControlURL, streamURL, opt)
	opt.Progress("Stop=%d SetURI=%d Play=%d", rep.Push.StopStatus, rep.Push.SetURIStatus, rep.Push.PlayStatus)

	// 4. 等 PLAYING，测量起播耗时。
	rep.Push.States, rep.Push.TimeToPlayingMS, rep.Push.TransitioningMS =
		waitPlaying(ctx, r.ControlURL, opt)
	if rep.Push.TimeToPlayingMS != nil {
		opt.Progress("进入 PLAYING，用时 %.1fs；持续拉流 %s 分钟",
			float64(*rep.Push.TimeToPlayingMS)/1000, opt.Duration)
	} else {
		opt.Progress("没有在 %s 内进入 PLAYING；继续记录拉流事件", opt.PlayTimeout)
	}

	// 5. 持续拉流 N 分钟。
	playDone := time.After(opt.Duration)
	select {
	case <-ctx.Done():
	case <-playDone:
	}
	httpObs, conns, disconnects, reconnects, firstDropAfter := events.snapshot()
	rep.Stream.HTTP = httpObs
	rep.Stream.Connections = conns
	rep.Stream.Disconnects = disconnects
	rep.Stream.Reconnects = reconnects
	rep.Stream.FirstDisconnectAfterMS = firstDropAfter

	// 6. 收尾：让电视回到 STOPPED，别把测试卡留在屏幕上。
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _, _ = sim.SoapCallRaw(stopCtx, r.ControlURL, sim.AVTService, "Stop", [][2]string{{"InstanceID", "0"}})
	cancel()

	rep.buildWarnings()
	return rep, nil
}

func makeStreamURL(listenAddr, host string) (string, error) {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		return "", fmt.Errorf("监听地址 %q 不合法：%v", listenAddr, err)
	}
	return "http://" + net.JoinHostPort(host, port) + "/live.avi", nil
}

func queryProtocolInfo(ctx context.Context, controlURL, serviceType string) ProtocolInfo {
	out := ProtocolInfo{VideoMIME: []string{}}
	if controlURL == "" || serviceType == "" {
		out.Error = "电视没有声明 ConnectionManager 服务"
		return out
	}
	status, raw, err := sim.SoapCallRaw(ctx, controlURL, serviceType, "GetProtocolInfo", nil)
	out.Status = status
	if err != nil {
		out.Error = err.Error()
		return out
	}
	var d struct {
		Source string `xml:"Body>GetProtocolInfoResponse>Source"`
		Sink   string `xml:"Body>GetProtocolInfoResponse>Sink"`
	}
	if status != http.StatusOK && out.Error == "" {
		out.Error = fmt.Sprintf("HTTP %d", status)
	}
	if err := xml.Unmarshal(raw, &d); err != nil {
		out.Error = "解析 GetProtocolInfo：" + err.Error()
		return out
	}
	out.Source, out.Sink = d.Source, d.Sink
	out.VideoMIME = videoMIMEs(d.Sink)
	return out
}

func videoMIMEs(sink string) []string {
	seen := map[string]bool{}
	var out []string
	for _, entry := range strings.Split(sink, ",") {
		parts := strings.Split(entry, ":")
		if len(parts) < 3 {
			continue
		}
		mime := strings.TrimSpace(parts[2])
		if !strings.HasPrefix(mime, "video/") || seen[mime] {
			continue
		}
		seen[mime] = true
		out = append(out, mime)
	}
	sort.Strings(out)
	return out
}

func pushStream(ctx context.Context, controlURL, streamURL string, opt Options) PushResult {
	var p PushResult
	p.StopStatus, _, _ = sim.SoapCallRaw(ctx, controlURL, sim.AVTService, "Stop", [][2]string{{"InstanceID", "0"}})
	select {
	case <-ctx.Done():
		return p
	case <-time.After(opt.StopSettle):
	}

	var err error
	p.SetURIStatus, _, err = sim.SoapCallRaw(ctx, controlURL, sim.AVTService, "SetAVTransportURI", [][2]string{
		{"InstanceID", "0"},
		{"CurrentURI", sim.XMLEscape(streamURL)},
		{"CurrentURIMetaData", sim.DIDL(streamURL, "StickBox TV Compatibility Check", "video/avi")},
	})
	if err != nil {
		p.SetURIError = err.Error()
	}
	p.PlayStatus, _, err = sim.SoapCallRaw(ctx, controlURL, sim.AVTService, "Play", [][2]string{
		{"InstanceID", "0"}, {"Speed", "1"},
	})
	if err != nil {
		p.PlayError = err.Error()
	}
	return p
}

// waitPlaying 轮询 GetTransportInfo，返回去重后的状态序列、到 PLAYING 的
// 耗时，以及 TRANSITIONING 持续时长（没观察到就为 nil）。
func waitPlaying(ctx context.Context, controlURL string, opt Options) ([]string, *int64, *int64) {
	playReturned := opt.Now()
	deadline := playReturned.Add(opt.PlayTimeout)
	var states []string
	var playingAt, transitionStart time.Time
	last := ""
	for {
		state, err := sim.TransportState(ctx, controlURL)
		if err == nil && state != "" {
			if state != last {
				states = append(states, state)
				last = state
			}
			if state == "PLAYING" {
				playingAt = opt.Now()
				break
			}
			if state == "TRANSITIONING" && transitionStart.IsZero() {
				transitionStart = opt.Now()
			}
		}
		if !opt.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return states, nil, nil
		case <-time.After(opt.PollInterval):
		}
	}
	if playingAt.IsZero() {
		return states, nil, nil
	}
	ms := playingAt.Sub(playReturned).Milliseconds()
	var trans *int64
	if !transitionStart.IsZero() {
		v := playingAt.Sub(transitionStart).Milliseconds()
		trans = &v
	}
	return states, &ms, trans
}

// ---- 事件记录 ----

type eventLog struct {
	mu        sync.Mutex
	now       func() time.Time
	start     time.Time
	conns     int
	drops     int
	lastDrop  time.Time
	firstConn time.Time
	firstDrop time.Time
	reconnect []Reconnect

	reqs       []RequestObs
	userAgents map[string]bool
	methods    map[string]bool
	sawHead    bool
	sawRange   bool
}

func newEventLog(now func() time.Time) *eventLog {
	return &eventLog{
		now: now, start: now(),
		userAgents: map[string]bool{}, methods: map[string]bool{},
	}
}

func (e *eventLog) hooks() sim.StreamEvents {
	return sim.StreamEvents{
		OnRequest: func(method string, h http.Header) {
			e.mu.Lock()
			defer e.mu.Unlock()
			ua := h.Get("User-Agent")
			rng := h.Get("Range")
			if rng == "" {
				rng = h.Get("TimeSeekRange.dlna.org")
			}
			e.methods[method] = true
			if ua != "" {
				e.userAgents[ua] = true
			}
			if method == http.MethodHead {
				e.sawHead = true
			}
			if rng != "" {
				e.sawRange = true
			}
			headers := map[string]string{}
			for _, k := range []string{
				"User-Agent", "Range", "transferMode.dlna.org",
				"getcontentFeatures.dlna.org", "getMediaInfo.sec", "Connection",
			} {
				if v := h.Get(k); v != "" {
					headers[k] = v
				}
			}
			if len(e.reqs) < 50 {
				e.reqs = append(e.reqs, RequestObs{
					Method: method, UserAgent: ua, Range: rng,
					AtMS: e.now().Sub(e.start).Milliseconds(), Headers: headers,
				})
			}
		},
		OnConnect: func(method string) {
			if method != http.MethodGet {
				return // HEAD 只是探测，不算拉流
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			e.conns++
			if e.firstConn.IsZero() {
				e.firstConn = e.now()
			}
			if !e.lastDrop.IsZero() {
				e.reconnect = append(e.reconnect, Reconnect{
					DowntimeMS: e.now().Sub(e.lastDrop).Milliseconds(),
				})
			}
		},
		OnDisconnect: func(method string) {
			if method != http.MethodGet {
				return
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			e.drops++
			now := e.now()
			if e.firstDrop.IsZero() {
				e.firstDrop = now
			}
			e.lastDrop = now
		},
	}
}

func (e *eventLog) snapshot() (HTTPObservations, int, int, []Reconnect, *int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var obs HTTPObservations
	obs.Requests = append([]RequestObs(nil), e.reqs...)
	for ua := range e.userAgents {
		obs.UserAgents = append(obs.UserAgents, ua)
	}
	for m := range e.methods {
		obs.Methods = append(obs.Methods, m)
	}
	sort.Strings(obs.UserAgents)
	sort.Strings(obs.Methods)
	obs.SawHead, obs.SawRange = e.sawHead, e.sawRange
	var firstDropAfter *int64
	if !e.firstConn.IsZero() && !e.firstDrop.IsZero() {
		v := e.firstDrop.Sub(e.firstConn).Milliseconds()
		firstDropAfter = &v
	}
	return obs, e.conns, e.drops, append([]Reconnect(nil), e.reconnect...), firstDropAfter
}

// ---- 判定规则 ----

func (r *Report) buildWarnings() {
	sev := func(id, severity, msg, hint string) {
		r.Warnings = append(r.Warnings, Warning{ID: id, Severity: severity, Message: msg, Hint: hint})
	}

	if r.Push.SetURIStatus == http.StatusPaymentRequired || r.Push.PlayStatus == http.StatusPaymentRequired {
		sev("numeric_entity_402", "error",
			"电视返回 402 Invalid Args",
			"三星 Room C 的 SOAP 解析器不认数字实体（&#34; / &#39;）。StickBox 从 T17 起只用 &amp; / &lt; / &gt; 转义；如果这里仍报 402，请把完整报告贴到 issue。")
	}
	for action, st := range map[string]int{
		"SetAVTransportURI": r.Push.SetURIStatus,
		"Play":              r.Push.PlayStatus,
	} {
		if st >= 300 && st != http.StatusPaymentRequired {
			sev("soap_"+strings.ToLower(action), "error",
				fmt.Sprintf("%s 返回 HTTP %d", action, st),
				"这是电视的 SOAP 服务拒绝或内部错误；附报告方便按机型排查。")
		}
	}
	if r.Push.TimeToPlayingMS == nil {
		sev("no_playing", "error", "15 秒内没有进入 PLAYING",
			"确认电视没有弹窗/待机；有些电视需要先在屏幕上允许投屏。附报告可看完整状态序列。")
	} else if r.Push.TransitioningMS != nil {
		ms := *r.Push.TransitioningMS
		if ms > 5000 {
			sev("transitioning_slow", "warn",
				fmt.Sprintf("TRANSITIONING 持续了 %.1fs 才进入 PLAYING", float64(ms)/1000),
				"三星正常是 1–2.5s；超过 5s 可能起播偏慢，记录机型并附报告。")
		} else {
			sev("transitioning", "info",
				fmt.Sprintf("Play 后先经历 TRANSITIONING，约 %.1fs 后 PLAYING", float64(ms)/1000),
				"这是正常现象（三星 1–2.5s），不是失败。")
		}
	}
	if r.Push.TimeToPlayingMS != nil && r.Stream.Connections == 0 {
		sev("no_stream_request", "warn", "进入 PLAYING 后没有收到电视拉流请求",
			"电视可能没有真正播放，或在等缓冲。把报告和电视型号贴到 issue。")
	}
	if r.Stream.Disconnects > 0 {
		sev("disconnects", "info",
			fmt.Sprintf("长跑期间断开 %d 次", r.Stream.Disconnects),
			reconnectHint(r.Stream.Reconnects))
	}
	if len(r.ProtocolInfo.VideoMIME) > 0 && !supportsAVI(r.ProtocolInfo.VideoMIME) {
		sev("mime_not_declared", "warn",
			"GetProtocolInfo 没有声明 video/avi",
			"三星通常仍然能播，但不保证；报告里保留电视声明的 MIME 列表。")
	}
	if r.ProtocolInfo.Error != "" {
		sev("protocol_info_error", "warn", "GetProtocolInfo 失败："+r.ProtocolInfo.Error,
			"不影响推流测试，但报告里缺少电视声明的 MIME。")
	}
	if r.Stream.HTTP.SawHead {
		sev("head_probe", "info", "电视先发过 HEAD 请求",
			"有些电视用它探测流；服务端对 HEAD 只回头不回帧，属于正常。")
	}
	if r.Stream.HTTP.SawRange {
		sev("range_request", "info", "电视请求带了 Range",
			"当前推流按 DLNA Streaming 不返回 Content-Length，Range 会被忽略；如果电视因此不播，附报告。")
	}
	if r.Stream.HTTP.Requests != nil && len(r.Stream.HTTP.UserAgents) == 0 {
		sev("no_user_agent", "info", "电视的 HTTP 请求没有 User-Agent", "")
	}
	// 首推断开：第一次连接后 15 秒内断开。
	if r.Stream.FirstDisconnectAfterMS != nil && *r.Stream.FirstDisconnectAfterMS < 15000 {
		sev("first_push_disconnect", "warn",
			fmt.Sprintf("疑似首推断开：首次拉流后 %.1fs 电视就断开了",
				msToSeconds(*r.Stream.FirstDisconnectAfterMS)),
			"T17 在三星上见过首推约 4 秒断开；设备用 burst 8（起播连发 8 帧）缓解。附报告和机型。")
	}
}

func reconnectHint(rs []Reconnect) string {
	if len(rs) == 0 {
		return "没有记录到重连。"
	}
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%.1fs", float64(r.DowntimeMS)/1000))
	}
	return "断开到重连的耗时：" + strings.Join(parts, "、") + "。"
}

func supportsAVI(mimes []string) bool {
	for _, m := range mimes {
		if m == "video/avi" || m == "video/x-msvideo" {
			return true
		}
	}
	return false
}

func msToSeconds(ms int64) float64 { return float64(ms) / 1000 }
