package main

// 局域网自动发现并配对（与固件 #28 的接口约定）：
//
//  1. 发现：向 255.255.255.255:5141 UDP 广播 `SBPAIR?1`，板子单播回
//     {"v":1,"dev":"<12hex>","pending":true,"fw":"<版本>","port":8080}；
//     收集 2 秒内的回复，只保留 pending=true、且**来源 IP 在私有地址段**的。
//  2. 读码：GET http://<板子ip>:<port>/pair → {"dev","code","expires_in"}
//     （只有待配对时才有，否则 404）。IP 只取 UDP 来源地址，不信回复里自报的任何地址。
//  3. 配对：拿到 code 后走 pairAndSave，与手输 `pair` 完全相同的代码路径。
//
// 码是一次性的敏感数据：不写日志、不进错误文本、不回传给网页。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	discoverAddr     = "255.255.255.255:5141"
	discoverProbe    = "SBPAIR?1"
	discoverWait     = 2 * time.Second
	discoverMaxFound = 16
	codeReadTimeout  = 3 * time.Second
)

var devIDRe = regexp.MustCompile(`^[0-9a-fA-F]{12}$`)

// foundDevice 是一块发现到的待配对设备。IP 来自 UDP 来源地址。
type foundDevice struct {
	Dev  string `json:"dev"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
	FW   string `json:"fw"`
}

// Short 是 dev 的后 4 位，页面和终端里用来辨认。
func (d foundDevice) Short() string {
	if len(d.Dev) <= 4 {
		return d.Dev
	}
	return d.Dev[len(d.Dev)-4:]
}

// discoverer / codeReader 做成接口，便于单测注入；真机联调由 master 完成。
type discoverer interface {
	Discover(ctx context.Context) ([]foundDevice, error)
}

type codeReader interface {
	// ReadCode 返回设备当前的 6 位配对码。不在待配对 → errNotPending；
	// 码已过期（expires_in<=0）→ errCodeExpired。
	ReadCode(ctx context.Context, d foundDevice) (string, error)
}

var (
	errNotPending  = errors.New("这块屏现在不在待配对状态（可能已经配好，或屏幕上没有显示配对码）")
	errCodeExpired = errors.New("屏幕上的配对码已过期，请等它刷新后重新搜索")
)

// isPrivateLAN：只接受 RFC1918 私有 IPv4（10/8、172.16/12、192.168/16）。
// 回环、链路本地、公网地址一律不收。
func isPrivateLAN(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	switch {
	case ip4[0] == 10:
		return true
	case ip4[0] == 172 && ip4[1]&0xf0 == 16:
		return true
	case ip4[0] == 192 && ip4[1] == 168:
		return true
	}
	return false
}

// ---- UDP 发现 ----

type udpDiscoverer struct {
	addr   string // 广播目标；单测里指向本地假板子
	wait   time.Duration
	accept func(net.IP) bool // 来源 IP 过滤；默认只收私有地址
}

func newUDPDiscoverer() *udpDiscoverer {
	return &udpDiscoverer{addr: discoverAddr, wait: discoverWait, accept: isPrivateLAN}
}

type discoverReply struct {
	V       int    `json:"v"`
	Dev     string `json:"dev"`
	Pending bool   `json:"pending"`
	FW      string `json:"fw"`
	Port    int    `json:"port"`
}

// parseReply 校验并转换一条回复；不合格返回 false。
func parseReply(raw []byte, src net.IP, accept func(net.IP) bool) (foundDevice, bool) {
	if accept != nil && !accept(src) {
		return foundDevice{}, false
	}
	var r discoverReply
	if json.Unmarshal(raw, &r) != nil {
		return foundDevice{}, false
	}
	if r.V != 1 || !r.Pending || !devIDRe.MatchString(r.Dev) || r.Port < 1 || r.Port > 65535 {
		return foundDevice{}, false
	}
	fw := truncateRunes(strings.TrimSpace(r.FW), 32)
	if strings.ContainsAny(fw, "\r\n\x00") {
		fw = ""
	}
	return foundDevice{Dev: strings.ToLower(r.Dev), IP: src.String(), Port: r.Port, FW: fw}, true
}

func (u *udpDiscoverer) Discover(ctx context.Context) ([]foundDevice, error) {
	dst, err := net.ResolveUDPAddr("udp4", u.addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("打开 UDP 失败：%w", err)
	}
	defer conn.Close()
	if _, err := conn.WriteToUDP([]byte(discoverProbe), dst); err != nil {
		return nil, fmt.Errorf("发送发现广播失败：%w", err)
	}
	deadline := time.Now().Add(u.wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	go func() { // ctx 取消时立刻打断读
		<-ctx.Done()
		_ = conn.SetReadDeadline(time.Now())
	}()

	found := map[string]foundDevice{}
	buf := make([]byte, 1024)
	for len(found) < discoverMaxFound {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // 超时即收齐
		}
		if d, ok := parseReply(buf[:n], from.IP, u.accept); ok {
			found[d.Dev] = d
		}
	}
	out := make([]foundDevice, 0, len(found))
	for _, d := range found {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dev < out[j].Dev })
	return out, nil
}

// ---- HTTP 读码 ----

type httpCodeReader struct {
	hc      *http.Client
	accept  func(net.IP) bool
	timeout time.Duration
}

func newHTTPCodeReader() *httpCodeReader {
	return &httpCodeReader{
		hc: &http.Client{
			Timeout: codeReadTimeout,
			// 不跟随重定向：板子不该把我们弹到别处。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		accept:  isPrivateLAN,
		timeout: codeReadTimeout,
	}
}

func (h *httpCodeReader) ReadCode(ctx context.Context, d foundDevice) (string, error) {
	ip := net.ParseIP(d.IP)
	if ip == nil || (h.accept != nil && !h.accept(ip)) {
		return "", errors.New("设备地址不是局域网私有地址，已拒绝")
	}
	if d.Port < 1 || d.Port > 65535 {
		return "", errors.New("设备端口不合法")
	}
	u := "http://" + net.JoinHostPort(d.IP, strconv.Itoa(d.Port)) + "/pair"
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "stickbox-agent/"+version)
	resp, err := h.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("连不上屏幕 %s（超时或被拒绝）", d.IP)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", errNotPending
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("屏幕返回了 HTTP %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	var out struct {
		Dev       string `json:"dev"`
		Code      string `json:"code"`
		ExpiresIn int    `json:"expires_in"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return "", errors.New("屏幕返回了无法识别的内容")
	}
	if !strings.EqualFold(out.Dev, d.Dev) {
		return "", errors.New("屏幕编号与发现时不一致，请重新搜索")
	}
	if !validPairCode(out.Code) {
		return "", errors.New("屏幕返回的配对码格式不对")
	}
	if out.ExpiresIn <= 0 {
		return "", errCodeExpired
	}
	return out.Code, nil
}

// pairFound 读码 + 走与手输 `pair` 完全相同的 pairAndSave。码不进日志和错误文本。
func pairFound(ctx context.Context, c *agentConfig, rd codeReader, d foundDevice, agentName string) (dev string, newUser bool, err error) {
	code, err := rd.ReadCode(ctx, d)
	if err != nil {
		return "", false, err
	}
	return pairAndSave(c, code, agentName)
}

// ---- 配置准备（pair / pair --auto / 网页共用） ----

// loadPairConfig 读配置并套用 --server / --insecure 覆盖，服务端地址规范化。
func loadPairConfig(server string, insecure bool) (*agentConfig, error) {
	c, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if server != "" {
		c.Server = server
	}
	if c.Server, err = normalizeServer(c.Server); err != nil {
		return nil, err
	}
	if insecure {
		c.Insecure = true
	}
	return c, nil
}

// ---- CLI ----

func printFound(out io.Writer, devs []foundDevice) {
	for i, d := range devs {
		fw := d.FW
		if fw == "" {
			fw = "未知"
		}
		fmt.Fprintf(out, "  %d) …%s  固件 %s  %s\n", i+1, d.Short(), fw, d.IP)
	}
}

func cmdDiscover(args []string) error {
	fs := newFlagSet("discover")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("用法：stickbox-agent discover")
	}
	return runDiscover(context.Background(), newUDPDiscoverer(), os.Stdout)
}

func runDiscover(ctx context.Context, d discoverer, out io.Writer) error {
	fmt.Fprintln(out, "正在局域网里搜索待配对的屏幕（约 2 秒）……")
	devs, err := d.Discover(ctx)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Fprintln(out, "没找到待配对的屏幕。请确认电视上正显示配对码、电脑与屏幕在同一局域网；也可以直接 stickbox-agent pair <6 位码>。")
		return nil
	}
	fmt.Fprintf(out, "找到 %d 块待配对的屏幕：\n", len(devs))
	printFound(out, devs)
	return nil
}

// pairAuto：发现 → 选择/确认 → 读码 → 配对。in/out 可注入。
func pairAuto(ctx context.Context, opts uiOptions, yes bool, d discoverer, rd codeReader, in io.Reader, out io.Writer) error {
	fmt.Fprintln(out, "正在局域网里搜索待配对的屏幕（约 2 秒）……")
	devs, err := d.Discover(ctx)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		return errors.New("没找到待配对的屏幕：请确认电视上正显示配对码、电脑与屏幕在同一局域网；或直接 stickbox-agent pair <6 位码>")
	}
	br := bufio.NewReader(in)
	ask := func(prompt string) (string, error) {
		fmt.Fprint(out, prompt)
		line, err := br.ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("已取消（没有收到输入）")
		}
		return strings.TrimSpace(line), nil
	}

	pick := devs[0]
	if len(devs) > 1 {
		fmt.Fprintf(out, "找到 %d 块待配对的屏幕：\n", len(devs))
		printFound(out, devs)
		if yes {
			return errors.New("找到多块屏幕，--yes 无法替你选：去掉 --yes 交互选择，或用 stickbox-agent pair <6 位码>")
		}
		ans, err := ask("要配哪一块？输入序号（回车取消）：")
		if err != nil {
			return err
		}
		n, convErr := strconv.Atoi(ans)
		if convErr != nil || n < 1 || n > len(devs) {
			return errors.New("已取消")
		}
		pick = devs[n-1]
	} else if !yes {
		fmt.Fprintf(out, "找到 1 块待配对的屏幕：\n")
		printFound(out, devs)
		ans, err := ask("现在配对它？[y/N] ")
		if err != nil {
			return err
		}
		if a := strings.ToLower(ans); a != "y" && a != "yes" {
			return errors.New("已取消")
		}
	}

	c, err := loadPairConfig(opts.server, opts.insecure)
	if err != nil {
		return err
	}
	dev, newUser, err := pairFound(ctx, c, rd, pick, resolveAgentName(opts.name))
	if err != nil {
		var pp postPairError
		if errors.As(err, &pp) {
			return err
		}
		if isUnauthorized(err) {
			return fmt.Errorf("本机保存的令牌已失效，先运行 stickbox-agent logout 再配对：%w", err)
		}
		return fmt.Errorf("配对失败：%w", err)
	}
	if newUser {
		fmt.Fprintf(out, "配对成功：%s（新用户）\n", dev)
	} else {
		fmt.Fprintf(out, "配对成功：%s（已并入现有账号）\n", dev)
	}
	fmt.Fprintln(out, "接下来：stickbox-agent run --once 试跑一轮，或 stickbox-agent install 开机常驻")
	return nil
}
