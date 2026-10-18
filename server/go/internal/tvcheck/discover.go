package tvcheck

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Renderer 是一台局域网 DLNA 渲染器。
type Renderer struct {
	Name         string `json:"name"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model,omitempty"`
	Location     string `json:"location"`
	// ControlURL 是 AVTransport 的控制地址。
	ControlURL string `json:"control_url,omitempty"`
	// ConnectionManagerURL / ConnectionManagerService 是 ConnectionManager
	// 的控制地址与服务类型。GetProtocolInfo 属于这个服务；发到 AVTransport
	// 或信封里写 AVTransport 的 xmlns 都会被三星回 500。
	ConnectionManagerURL     string `json:"connection_manager_url,omitempty"`
	ConnectionManagerService string `json:"connection_manager_service,omitempty"`
	IP                       string `json:"ip"`
}

// Display 是给用户看的一行描述。
func (r Renderer) Display() string {
	var parts []string
	if r.Manufacturer != "" {
		parts = append(parts, r.Manufacturer)
	}
	if r.Model != "" && r.Model != r.Name {
		parts = append(parts, r.Model)
	}
	detail := strings.Join(parts, " / ")
	if detail == "" {
		detail = r.IP
	}
	return fmt.Sprintf("%s（%s）", r.Name, detail)
}

// Discover 用 SSDP 找局域网里的 MediaRenderer，再读设备描述拿 AVTransport
// 控制地址。没有 AVTransport 的渲染器也会返回，ControlURL 为空。
func Discover(ctx context.Context, timeout time.Duration) ([]Renderer, error) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil, fmt.Errorf("打开 SSDP 端口：%w", err)
	}
	defer conn.Close()

	dst, err := net.ResolveUDPAddr("udp4", "239.255.255.250:1900")
	if err != nil {
		return nil, err
	}
	req := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n"

	// 连续发三次：UDP 会丢，设备也可能刚睡醒。
	stop := time.Now().Add(timeout)
	for i := 0; i < 3; i++ {
		if _, err := conn.WriteTo([]byte(req), dst); err != nil {
			return nil, fmt.Errorf("发送 SSDP 搜索：%w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}

	locations := map[string]bool{}
	var ordered []string
	buf := make([]byte, 4096)
	for {
		remain := time.Until(stop)
		if remain <= 0 {
			break
		}
		conn.SetReadDeadline(time.Now().Add(remain))
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break // 超时或关闭，结束收集
		}
		loc := headerValue(string(buf[:n]), "LOCATION")
		if loc == "" || locations[loc] {
			continue
		}
		locations[loc] = true
		ordered = append(ordered, loc)
	}

	hc := &http.Client{Timeout: 5 * time.Second}
	var out []Renderer
	for _, loc := range ordered {
		r, err := fetchRenderer(ctx, hc, loc)
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].IP < out[j].IP
		}
		return out[i].Name < out[j].Name
	})
	if len(out) == 0 {
		return nil, fmt.Errorf("没有发现 DLNA 渲染器（电视需开机、与电脑同一局域网、DMR 已开启）")
	}
	return out, nil
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

type deviceDescription struct {
	Device struct {
		FriendlyName string `xml:"friendlyName"`
		Manufacturer string `xml:"manufacturer"`
		ModelName    string `xml:"modelName"`
		UDN          string `xml:"UDN"`
		ServiceList  []struct {
			ServiceType string `xml:"serviceType"`
			ControlURL  string `xml:"controlURL"`
		} `xml:"serviceList>service"`
	} `xml:"device"`
}

func fetchRenderer(ctx context.Context, hc *http.Client, location string) (Renderer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return Renderer{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Renderer{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Renderer{}, err
	}
	r, err := parseDescription(location, raw)
	if err != nil {
		return Renderer{}, err
	}
	return r, nil
}

// parseDescription 从设备描述 XML 里取名称、厂商、型号和 AVTransport 控制地址。
func parseDescription(location string, raw []byte) (Renderer, error) {
	var d deviceDescription
	if err := xml.Unmarshal(raw, &d); err != nil {
		return Renderer{}, err
	}
	base, err := url.Parse(location)
	if err != nil {
		return Renderer{}, err
	}
	r := Renderer{
		Name:         strings.TrimSpace(d.Device.FriendlyName),
		Manufacturer: strings.TrimSpace(d.Device.Manufacturer),
		Model:        strings.TrimSpace(d.Device.ModelName),
		Location:     location,
		IP:           base.Hostname(),
	}
	if r.Name == "" {
		r.Name = base.Hostname()
	}
	for _, s := range d.Device.ServiceList {
		ref, err := url.Parse(s.ControlURL)
		if err != nil {
			continue
		}
		u := base.ResolveReference(ref).String()
		switch {
		case strings.Contains(s.ServiceType, "AVTransport") && r.ControlURL == "":
			r.ControlURL = u
		case strings.Contains(s.ServiceType, "ConnectionManager") && r.ConnectionManagerURL == "":
			r.ConnectionManagerURL = u
			r.ConnectionManagerService = s.ServiceType
		}
	}
	return r, nil
}
