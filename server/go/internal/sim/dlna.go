package sim

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const avtService = "urn:schemas-upnp-org:service:AVTransport:1"

// stopSettle 是 Stop 之后等电视真的进入 STOPPED 的时间。
// 三星从 PLAYING 切到 STOPPED 要 200–500ms，不等就 SetAVTransportURI 会 701。
const stopSettle = 500 * time.Millisecond

// 三星 Play 之后一般要 1–2.5s 才进 PLAYING，这期间是 TRANSITIONING。
// 只读一次会把刚起播的流当成失败，Stop 重推等于每次自己掐断。
const (
	playPollInterval = 500 * time.Millisecond
	playTimeout      = 15 * time.Second
	minStreamAge     = 5 * time.Second
)

// streamProbe 提供电视拉 /live.avi 的连接状态。
// 比 SOAP 状态码更可靠：真的连上来拉流了才算成功。
type streamProbe interface {
	// ActiveFor 返回最久的拉流连接已经持续了多久。
	ActiveFor() time.Duration
	// Active 表示当前有连接仍在正常收帧（不是半开的僵尸连接）。
	Active() bool
}

type pushParams struct {
	stopSettle   time.Duration
	poll         time.Duration
	playTimeout  time.Duration
	minStreamAge time.Duration
}

var defaultPushParams = pushParams{
	stopSettle:   stopSettle,
	poll:         playPollInterval,
	playTimeout:  playTimeout,
	minStreamAge: minStreamAge,
}

// discoverAVTransport 用 SSDP 找 MediaRenderer，再读设备描述里的
// AVTransport controlURL。targetIP 非空时只接受该地址的响应。
func discoverAVTransport(ctx context.Context, targetIP, name string) (string, error) {
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return "", err
	}
	defer conn.Close()

	dst, err := net.ResolveUDPAddr("udp4", "239.255.255.250:1900")
	if err != nil {
		return "", err
	}
	req := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n"
	if _, err := conn.WriteTo([]byte(req), dst); err != nil {
		return "", err
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	hc := &http.Client{Timeout: 5 * time.Second}
	seen := map[string]bool{}
	for {
		buf := make([]byte, 2048)
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break
		}
		loc := headerValue(string(buf[:n]), "LOCATION")
		if loc == "" || seen[loc] {
			continue
		}
		seen[loc] = true
		ctrl, friendly, err := fetchAVControl(ctx, hc, loc)
		if err != nil {
			continue
		}
		u, err := url.Parse(loc)
		if err != nil {
			continue
		}
		if targetIP != "" && u.Hostname() != targetIP {
			continue
		}
		if name != "" && !strings.Contains(strings.ToLower(friendly), strings.ToLower(name)) {
			continue
		}
		return ctrl, nil
	}
	return "", fmt.Errorf("sim: 没有找到带 AVTransport 的 DLNA 设备")
}

func headerValue(msg, key string) string {
	for _, line := range strings.Split(msg, "\r\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func fetchAVControl(ctx context.Context, hc *http.Client, loc string) (control, friendly string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	var d struct {
		Device struct {
			FriendlyName string `xml:"friendlyName"`
			ServiceList  []struct {
				ServiceType string `xml:"serviceType"`
				ControlURL  string `xml:"controlURL"`
			} `xml:"serviceList>service"`
		} `xml:"device"`
	}
	if err := xml.Unmarshal(raw, &d); err != nil {
		return "", "", err
	}
	base, err := url.Parse(loc)
	if err != nil {
		return "", "", err
	}
	for _, s := range d.Device.ServiceList {
		if !strings.Contains(s.ServiceType, "AVTransport") {
			continue
		}
		ref, err := url.Parse(s.ControlURL)
		if err != nil {
			return "", "", err
		}
		return base.ResolveReference(ref).String(), d.Device.FriendlyName, nil
	}
	return "", d.Device.FriendlyName, fmt.Errorf("sim: 设备没有 AVTransport 服务")
}

// soapEnvelope 拼 SOAP 请求体。参数值由调用方转义后原样放入（和
// dlna_push.py 一致）——DIDL 本身已经转义过一遍。
func soapEnvelope(action string, args [][2]string) string {
	var body strings.Builder
	for _, kv := range args {
		fmt.Fprintf(&body, "<%s>%s</%s>", kv[0], kv[1], kv[0])
	}
	return `<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>` +
		`<u:` + action + ` xmlns:u="` + avtService + `">` + body.String() +
		`</u:` + action + `></s:Body></s:Envelope>`
}

func soapCall(ctx context.Context, controlURL, action string, args [][2]string) error {
	_, err := soapDo(ctx, controlURL, action, args)
	return err
}

// soapDo 发 SOAP 请求并返回响应体。
func soapDo(ctx context.Context, controlURL, action string, args [][2]string) ([]byte, error) {
	env := soapEnvelope(action, args)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controlURL, strings.NewReader(env))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", `"`+avtService+`#`+action+`"`)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		// 图片/直播流没有可 Play 的状态迁移时电视回 701，不算失败。
		if (action == "Play" || action == "Stop" || action == "Next") &&
			bytes.Contains(raw, []byte("<errorCode>701<")) {
			return raw, nil
		}
		return raw, fmt.Errorf("sim: %s 失败 %d：%s", action, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

// xmlEscape 只转义 & < >。不能用 xml.EscapeText：它会把引号写成
// &#34; / &#39;，三星 Room C 的 SOAP 解析器不认数字实体（402）。
// 这里与 T17 dlna_push.py 用的 saxutils.escape 一致。
var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func xmlEscape(s string) string {
	return xmlEscaper.Replace(s)
}

// transportState 读 AVTransport 当前状态（PLAYING / STOPPED / …）。
func transportState(ctx context.Context, controlURL string) (string, error) {
	raw, err := soapDo(ctx, controlURL, "GetTransportInfo", [][2]string{{"InstanceID", "0"}})
	if err != nil {
		return "", err
	}
	var r struct {
		State string `xml:"Body>GetTransportInfoResponse>CurrentTransportState"`
	}
	if err := xml.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("sim: 解析 GetTransportInfo：%w", err)
	}
	if r.State == "" {
		return "", fmt.Errorf("sim: GetTransportInfo 没有 CurrentTransportState：%s", strings.TrimSpace(string(raw)))
	}
	return r.State, nil
}

func didl(url, title, mime string) string {
	class := "object.item.imageItem.photo"
	if strings.HasPrefix(mime, "video/") {
		class = "object.item.videoItem"
	}
	item := `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" ` +
		`xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
		`xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">` +
		`<item id="0" parentID="-1" restricted="1">` +
		`<dc:title>` + xmlEscape(title) + `</dc:title>` +
		`<upnp:class>` + class + `</upnp:class>` +
		`<res protocolInfo="http-get:*:` + mime + `:*">` + xmlEscape(url) + `</res>` +
		`</item></DIDL-Lite>`
	return xmlEscape(item)
}

// pushDLNA 把直播流地址推给电视（Stop → SetAVTransportURI → Play），
// 再等电视真的进 PLAYING 并开始拉流。
// SSDP 找不到时回落到 T17 实测的三星控制地址。
func pushDLNA(ctx context.Context, tvIP, streamURL string, probe streamProbe) error {
	control, err := discoverAVTransport(ctx, tvIP, "")
	if err != nil {
		control = "http://" + net.JoinHostPort(tvIP, "9197") + "/upnp/control/AVTransport1"
	}
	return pushToControl(ctx, control, streamURL, probe, defaultPushParams)
}

func pushToControl(ctx context.Context, controlURL, streamURL string, probe streamProbe, p pushParams) error {
	_ = soapCall(ctx, controlURL, "Stop", [][2]string{{"InstanceID", "0"}})
	select {
	case <-time.After(p.stopSettle):
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := soapCall(ctx, controlURL, "SetAVTransportURI", [][2]string{
		{"InstanceID", "0"},
		{"CurrentURI", xmlEscape(streamURL)},
		{"CurrentURIMetaData", didl(streamURL, "StickBox", "video/avi")},
	}); err != nil {
		return err
	}
	if err := soapCall(ctx, controlURL, "Play", [][2]string{
		{"InstanceID", "0"}, {"Speed", "1"},
	}); err != nil {
		return err
	}
	return waitForPlaying(ctx, controlURL, probe, p)
}

// waitForPlaying 轮询 GetTransportInfo：TRANSITIONING 继续等，PLAYING 后
// 还要有一条持续 minStreamAge 的拉流连接才算成功；STOPPED /
// NO_MEDIA_PRESENT 或超时算失败。
func waitForPlaying(ctx context.Context, controlURL string, probe streamProbe, p pushParams) error {
	deadline := time.Now().Add(p.playTimeout)
	var last string
	var lastErr error
	for {
		state, err := transportState(ctx, controlURL)
		if err == nil {
			last, lastErr = state, nil
			switch state {
			case "PLAYING":
				if probe == nil || probe.ActiveFor() >= p.minStreamAge {
					return nil
				}
			case "STOPPED", "NO_MEDIA_PRESENT":
				return fmt.Errorf("sim: 推送后电视状态是 %s，播放未生效", state)
			}
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			if last == "" && lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("sim: %s 内没等到 PLAYING + 持续 %s 的拉流连接（最后状态 %q）",
				p.playTimeout, p.minStreamAge, last)
		}
		select {
		case <-time.After(p.poll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
