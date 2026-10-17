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

func soapCall(ctx context.Context, controlURL, action string, args [][2]string) error {
	var body strings.Builder
	for _, kv := range args {
		fmt.Fprintf(&body, "<%s>%s</%s>", kv[0], kv[1], kv[0])
	}
	env := `<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>` +
		`<u:` + action + ` xmlns:u="` + avtService + `">` + body.String() +
		`</u:` + action + `></s:Body></s:Envelope>`

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controlURL, strings.NewReader(env))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", `"`+avtService+`#`+action+`"`)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		// 图片/直播流没有可 Play 的状态迁移时电视回 701，不算失败。
		if (action == "Play" || action == "Stop" || action == "Next") &&
			bytes.Contains(raw, []byte("<errorCode>701<")) {
			return nil
		}
		return fmt.Errorf("sim: %s 失败 %d：%s", action, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
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

// pushDLNA 把直播流地址推给电视（Stop → SetAVTransportURI → Play）。
// SSDP 找不到时回落到 T17 实测的三星控制地址。
func pushDLNA(ctx context.Context, tvIP, streamURL string) error {
	control, err := discoverAVTransport(ctx, tvIP, "")
	if err != nil {
		control = "http://" + net.JoinHostPort(tvIP, "9197") + "/upnp/control/AVTransport1"
	}
	_ = soapCall(ctx, control, "Stop", [][2]string{{"InstanceID", "0"}})
	if err := soapCall(ctx, control, "SetAVTransportURI", [][2]string{
		{"InstanceID", "0"},
		{"CurrentURI", xmlEscape(streamURL)},
		{"CurrentURIMetaData", didl(streamURL, "StickBox", "video/avi")},
	}); err != nil {
		return err
	}
	return soapCall(ctx, control, "Play", [][2]string{
		{"InstanceID", "0"}, {"Speed", "1"},
	})
}
