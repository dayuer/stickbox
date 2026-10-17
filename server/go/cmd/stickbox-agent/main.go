// stickbox-agent 是用户电脑上的用量上报代理。
//
// 它用用户**自己的** Claude 登录执行 `claude -p /usage`，把解析出的用量
// 用 agent 令牌 POST 给服务端；服务端再经 MQTT 推给用户名下的屏幕。
//
// 设计约束（都实测过，见 docs/PLAN.md T2）：
//   - 宿主环境里的 ANTHROPIC_* / CLAUDE_* 变量会把 claude 指向第三方接口或
//     注入宿主凭据，子进程环境必须剔掉；
//   - 用户 settings.json 也可能改写 CLI 的接口地址，所以固定带
//     --setting-sources project；
//   - 用量接口 429 后客户端本地冷却约 1 小时，期间 rate_limits 为 null，
//     这一轮跳过即可，不是错误；
//   - launchd / systemd 的 PATH 很短，install 时把 claude 的绝对路径写进配置；
//   - resets_at 是 ISO 8601，可能带小数秒和 +00:00。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	version       = "0.1.0"
	defaultServer = "https://mqtt.stickbox.ai"
	usageInterval = 10 * time.Minute
	claudeTimeout = 3 * time.Minute
	httpTimeout   = 20 * time.Second
)

// agentConfig 是用户配置目录里保存的全部状态。
type agentConfig struct {
	Server     string `json:"server,omitempty"`
	Token      string `json:"token,omitempty"`
	ClaudePath string `json:"claude_path,omitempty"`
	Name       string `json:"name,omitempty"`
	PairedAt   int64  `json:"paired_at,omitempty"`
	Insecure   bool   `json:"insecure,omitempty"`
}

// ---- 用量解析 ----

// usageWindow 与服务端 internal/usage.Window 的字段一致。
type usageWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"` // Unix 秒
}

type usagePayload struct {
	FiveHour *usageWindow    `json:"five_hour,omitempty"`
	SevenDay *usageWindow    `json:"seven_day,omitempty"`
	Limits   json.RawMessage `json:"limits,omitempty"`
}

// usageReportWire 对应 claude stream-json 里 assistant 事件的 usage_report。
type usageReportWire struct {
	RateLimits *rateLimitsWire `json:"rate_limits"`
}

type rateLimitsWire struct {
	Limits json.RawMessage `json:"limits"`
}

// limitRow 是 /usage 的 rate_limits.limits[] 一行。
// percent / resets_at 的准确名字取自 CLI 内置 schema（2.1.296）。
type limitRow struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	ResetsAt *string `json:"resets_at"`
	Severity string  `json:"severity"`
	IsActive bool    `json:"is_active"`
}

func (r limitRow) window() (*usageWindow, error) {
	if r.ResetsAt == nil || strings.TrimSpace(*r.ResetsAt) == "" {
		return nil, errors.New("resets_at 缺失")
	}
	ts, err := parseResetsAt(*r.ResetsAt)
	if err != nil {
		return nil, err
	}
	return &usageWindow{UsedPercentage: r.Percent, ResetsAt: ts}, nil
}

// parseUsageStream 从 `claude ... --output-format stream-json` 的逐行输出里
// 取最后一个带 usage_report 的事件。
//
// 返回的 skipReason 非空表示「本轮不报」——冷却期、没有限额行等，都不是错误。
func parseUsageStream(r io.Reader) (usagePayload, string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)

	var report *usageReportWire
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev struct {
			Type        string           `json:"type"`
			UsageReport *usageReportWire `json:"usage_report"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // 非 JSON 的诊断行，忽略
		}
		if ev.UsageReport != nil {
			report = ev.UsageReport
		}
	}
	if err := sc.Err(); err != nil {
		return usagePayload{}, "", fmt.Errorf("读取 claude 输出失败：%w", err)
	}
	if report == nil {
		return usagePayload{}, "", errors.New("claude 输出里没有 usage_report（/usage 的输出格式可能变了）")
	}
	if report.RateLimits == nil {
		return usagePayload{}, "用量接口暂时不可用（rate_limits 为 null，通常是刚触发过 429 冷却），本轮跳过", nil
	}
	return mapRateLimits(report.RateLimits.Limits)
}

func mapRateLimits(raw json.RawMessage) (usagePayload, string, error) {
	var rep usagePayload
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return rep, "claude 没有返回限额行（rate_limits.limits 为 null），本轮跳过", nil
	}
	var rows []limitRow
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		return rep, "", fmt.Errorf("解析 rate_limits.limits 失败：%w", err)
	}
	if len(rows) == 0 {
		return rep, "claude 返回的限额行为空，本轮跳过", nil
	}
	for _, row := range rows {
		switch row.Kind {
		case "session":
			if rep.FiveHour == nil {
				if w, err := row.window(); err == nil {
					rep.FiveHour = w
				}
			}
		case "weekly_all":
			if rep.SevenDay == nil {
				if w, err := row.window(); err == nil {
					rep.SevenDay = w
				}
			}
		}
	}
	if rep.FiveHour == nil && rep.SevenDay == nil {
		return rep, "限额行里没有可用的 session / weekly_all 窗口，本轮跳过", nil
	}
	rep.Limits = raw // 原始 limits 一并上报，服务端存给 App 看
	return rep, "", nil
}

// parseResetsAt 解析 ISO 8601；RFC3339Nano 同时接受小数秒、Z 和 +00:00。
func parseResetsAt(s string) (int64, error) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("无法解析 resets_at %q：%w", s, err)
	}
	return t.Unix(), nil
}

// filterEnv 去掉会污染子进程的宿主变量。
// CLAUDECODE 没有下划线前缀，单独列出来。
func filterEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "ANTHROPIC_") ||
			strings.HasPrefix(name, "CLAUDE_") ||
			name == "CLAUDECODE" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ---- 配置 ----

func configDir() (string, error) {
	if d := os.Getenv("STICKBOX_CONFIG_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("找不到用户配置目录：%w", err)
	}
	return filepath.Join(base, "stickbox"), nil
}

func configPath() (string, error) {
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "agent.json"), nil
}

func loadConfig() (*agentConfig, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &agentConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读配置失败：%w", err)
	}
	var c agentConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("配置文件损坏（%s）：%w", path, err)
	}
	return &c, nil
}

// saveConfig 原子写入，令牌文件权限固定 0600。
func saveConfig(c *agentConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败：%w", err)
	}
	_ = os.Chmod(dir, 0o700) // Windows 上忽略

	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(dir, ".agent-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

// ---- HTTP ----

type apiError struct {
	Status int
	Code   string
	Detail string
}

var errText = map[string]string{
	"unauthorized": "令牌无效",
	"bad code":     "配对码无效或已过期",
	"rate limited": "尝试次数过多，请稍后再试",
	"device bound": "该屏已绑定到其他用户",
	"not found":    "服务端没有这条数据",
	"forbidden":    "无权操作这台设备",
	"bad request":  "服务端拒绝了请求",
}

func (e *apiError) Error() string {
	msg := e.Code
	if t, ok := errText[e.Code]; ok {
		msg = t
	} else if msg == "" {
		msg = fmt.Sprintf("HTTP %d", e.Status)
	}
	if e.Detail != "" {
		msg += "（" + e.Detail + "）"
	}
	return msg
}

func isUnauthorized(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusUnauthorized
}

type apiClient struct {
	base  string
	token string
	hc    *http.Client
}

func normalizeServer(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultServer, nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("服务端地址必须以 http:// 或 https:// 开头：%q", s)
	}
	return strings.TrimRight(s, "/"), nil
}

func newAPIClient(c *agentConfig) (*apiClient, error) {
	base, err := normalizeServer(c.Server)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if c.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // 仅自签名证书时
	}
	return &apiClient{
		base:  base,
		token: c.Token,
		hc:    &http.Client{Timeout: httpTimeout, Transport: tr},
	}, nil
}

func (c *apiClient) call(method, path string, withAuth bool, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "stickbox-agent/"+version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withAuth && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接 %s 失败：%w", c.base, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		var e struct {
			E      string `json:"e"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(raw, &e)
		return &apiError{Status: resp.StatusCode, Code: e.E, Detail: e.Detail}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("服务端返回了无法识别的 JSON：%w", err)
		}
	}
	return nil
}

// ---- 服务端操作 ----

func pairDevice(c *agentConfig, code, name string) (dev, token string, err error) {
	cl, err := newAPIClient(c)
	if err != nil {
		return "", "", err
	}
	var out struct {
		Dev   string `json:"dev"`
		Token string `json:"token"`
	}
	// 已有令牌就带上：服务端会把新屏并入同一用户，不再签发新令牌。
	err = cl.call("POST", "/api/agent/pair", c.Token != "",
		map[string]string{"code": code, "name": name}, &out)
	return out.Dev, out.Token, err
}

func postUsage(c *agentConfig, rep usagePayload) (int, error) {
	cl, err := newAPIClient(c)
	if err != nil {
		return 0, err
	}
	var out struct {
		Devices int `json:"devices"`
	}
	if err := cl.call("POST", "/api/usage", true, rep, &out); err != nil {
		return 0, err
	}
	return out.Devices, nil
}

// ---- claude 子进程 ----

func resolveClaude(c *agentConfig) (string, error) {
	if c.ClaudePath != "" {
		if _, err := os.Stat(c.ClaudePath); err != nil {
			return "", fmt.Errorf("配置里的 claude 路径不可用（%s）：%w", c.ClaudePath, err)
		}
		return c.ClaudePath, nil
	}
	p, err := exec.LookPath("claude")
	if err != nil {
		return "", errors.New("PATH 里找不到 claude，请先安装并登录 Claude Code")
	}
	return p, nil
}

func collectUsage(c *agentConfig) (usagePayload, string, error) {
	claude, err := resolveClaude(c)
	if err != nil {
		return usagePayload{}, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, claude,
		"-p", "/usage",
		"--setting-sources", "project",
		"--no-session-persistence",
		"--output-format", "stream-json",
		"--verbose",
	)
	cmd.Env = filterEnv(os.Environ())
	// 固定工作目录，避免不同 launchd/systemd 环境下的 project 设置不一样。
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}

	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return usagePayload{}, "", fmt.Errorf("claude 执行超时（%s）", claudeTimeout)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr := strings.TrimSpace(string(ee.Stderr))
			if len(stderr) > 600 {
				stderr = stderr[len(stderr)-600:]
			}
			if stderr != "" {
				return usagePayload{}, "", fmt.Errorf("claude 执行失败：%v（%s）", err, stderr)
			}
		}
		return usagePayload{}, "", fmt.Errorf("claude 执行失败：%w", err)
	}
	return parseUsageStream(bytes.NewReader(out))
}

// runRound 执行一轮：取用量 → 上报。skip 不是错误。
func runRound(c *agentConfig, logf func(string, ...any)) error {
	rep, skip, err := collectUsage(c)
	if err != nil {
		return err
	}
	if skip != "" {
		logf("%s", skip)
		return nil
	}
	n, err := postUsage(c, rep)
	if err != nil {
		return fmt.Errorf("上报失败：%w", err)
	}
	logf("已上报：五小时 %s / 七天 %s；服务端推送到 %d 台设备",
		fmtWindow(rep.FiveHour), fmtWindow(rep.SevenDay), n)
	return nil
}

func fmtWindow(w *usageWindow) string {
	if w == nil {
		return "—"
	}
	return fmt.Sprintf("%s%%（%s 重置）",
		strconv.FormatFloat(w.UsedPercentage, 'f', -1, 64),
		time.Unix(w.ResetsAt, 0).Local().Format("2006-01-02 15:04"))
}

// ---- 命令 ----

var errNotPaired = errors.New("尚未配对：先运行 stickbox-agent pair <配对码>")

func requireToken(c *agentConfig) error {
	if c.Token == "" {
		return errNotPaired
	}
	return nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func cmdPair(args []string) error {
	fs := newFlagSet("pair")
	server := fs.String("server", "", "服务端地址（默认 "+defaultServer+"）")
	name := fs.String("name", "", "这台电脑的名称（默认主机名）")
	insecure := fs.Bool("insecure", false, "跳过 TLS 证书校验（仅自签名证书时使用）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("用法：stickbox-agent pair [--server URL] [--name 名称] [--insecure] <6 位配对码>")
	}
	code := strings.TrimSpace(fs.Arg(0))
	if len(code) != 6 || strings.Trim(code, "0123456789") != "" {
		return errors.New("配对码应为 6 位数字")
	}

	c, err := loadConfig()
	if err != nil {
		return err
	}
	if *server != "" {
		c.Server = *server
	}
	if c.Server, err = normalizeServer(c.Server); err != nil {
		return err
	}
	if *insecure {
		c.Insecure = true
	}

	agentName := strings.TrimSpace(*name)
	if agentName == "" {
		agentName, _ = os.Hostname()
	}
	if agentName == "" {
		agentName = "unknown"
	}
	agentName = truncateRunes(agentName, 64)

	dev, token, err := pairDevice(c, code, agentName)
	if err != nil {
		if isUnauthorized(err) {
			return fmt.Errorf("本机保存的令牌已失效，先运行 stickbox-agent logout 再配对：%w", err)
		}
		return fmt.Errorf("配对失败：%w", err)
	}
	if token != "" {
		c.Token = token
		c.Name = agentName
		c.PairedAt = time.Now().Unix()
	}
	if c.Token == "" {
		return errors.New("服务端没有返回 agent 令牌")
	}
	if err := saveConfig(c); err != nil {
		return err
	}
	if token != "" {
		fmt.Printf("配对成功：%s（新用户）\n", dev)
	} else {
		fmt.Printf("配对成功：%s（已并入现有账号）\n", dev)
	}
	fmt.Println("接下来：stickbox-agent run --once 试跑一轮，或 stickbox-agent install 开机常驻")
	return nil
}

func cmdUnpair(args []string) error {
	fs := newFlagSet("unpair")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("用法：stickbox-agent unpair <dev>")
	}
	dev := strings.TrimSpace(fs.Arg(0))

	c, err := loadConfig()
	if err != nil {
		return err
	}
	if err := requireToken(c); err != nil {
		return err
	}
	cl, err := newAPIClient(c)
	if err != nil {
		return err
	}
	if err := cl.call("POST", "/api/agent/unpair", true, map[string]string{"dev": dev}, nil); err != nil {
		return fmt.Errorf("解绑失败：%w", err)
	}
	fmt.Printf("已解绑 %s；屏幕会重新显示配对码\n", dev)
	return nil
}

type deviceRow struct {
	Dev    string `json:"dev"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
	Bound  int64  `json:"bound"`
}

func cmdDevices(args []string) error {
	fs := newFlagSet("devices")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if err := requireToken(c); err != nil {
		return err
	}
	cl, err := newAPIClient(c)
	if err != nil {
		return err
	}
	var out struct {
		Devices []deviceRow `json:"devices"`
	}
	if err := cl.call("GET", "/api/agent/devices", true, nil, &out); err != nil {
		return fmt.Errorf("查询设备失败：%w", err)
	}
	if len(out.Devices) == 0 {
		fmt.Println("名下还没有屏幕。")
		return nil
	}
	for _, d := range out.Devices {
		name := d.Name
		if name == "" {
			name = "（未命名）"
		}
		state := "离线"
		if d.Online {
			state = "在线"
		}
		line := fmt.Sprintf("%s  %s  %s", d.Dev, name, state)
		if d.Bound > 0 {
			line += "  绑定于 " + time.Unix(d.Bound, 0).Local().Format("2006-01-02 15:04")
		}
		fmt.Println(line)
	}
	return nil
}

func cmdRun(args []string) error {
	fs := newFlagSet("run")
	once := fs.Bool("once", false, "只跑一轮后退出")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if err := requireToken(c); err != nil {
		return err
	}

	logf := func(format string, a ...any) { log.Printf(format, a...) }
	if *once {
		return runRound(c, logf)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("stickbox-agent %s 启动，每 %s 上报一次", version, usageInterval)
	if err := runRound(c, logf); err != nil {
		log.Printf("本轮失败：%v（%s 后重试）", err, usageInterval)
	}
	ticker := time.NewTicker(usageInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("收到退出信号，停止")
			return nil
		case <-ticker.C:
			if err := runRound(c, logf); err != nil {
				log.Printf("本轮失败：%v", err)
			}
		}
	}
}

func cmdInstall(args []string) error {
	fs := newFlagSet("install")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if err := requireToken(c); err != nil {
		return err
	}
	claude, err := resolveClaude(c)
	if err != nil {
		return fmt.Errorf("%w（先安装并登录 Claude Code，再重试）", err)
	}
	absClaude, err := filepath.Abs(claude)
	if err != nil {
		return err
	}
	// launchd / systemd 的 PATH 很短，必须写绝对路径。
	c.ClaudePath = absClaude
	if err := saveConfig(c); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("找不到本程序的路径：%w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	return installService(exe)
}

func cmdUninstall(args []string) error {
	fs := newFlagSet("uninstall")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return uninstallService()
}

func cmdStatus(args []string) error {
	fs := newFlagSet("status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	path, _ := configPath()
	server, _ := normalizeServer(c.Server)

	fmt.Printf("stickbox-agent %s\n", version)
	fmt.Printf("服务器：%s\n", server)
	fmt.Printf("配置：%s\n", path)
	if c.Token == "" {
		fmt.Println("配对：未配对")
	} else {
		paired := ""
		if c.PairedAt > 0 {
			paired = "（" + time.Unix(c.PairedAt, 0).Local().Format("2006-01-02 15:04") + "）"
		}
		fmt.Printf("配对：已保存令牌%s\n", paired)
	}
	if c.Insecure {
		fmt.Println("TLS：已关闭证书校验（--insecure）")
	}
	if c.ClaudePath != "" {
		fmt.Printf("claude：%s\n", c.ClaudePath)
	} else if p, err := exec.LookPath("claude"); err == nil {
		fmt.Printf("claude：%s（安装后台服务时写入绝对路径）\n", p)
	} else {
		fmt.Println("claude：PATH 里没找到")
	}
	if installed, where := serviceInstalled(); installed {
		fmt.Printf("后台服务：已安装（%s）\n", where)
	} else {
		fmt.Println("后台服务：未安装（运行 stickbox-agent install）")
	}
	return nil
}

func cmdLogout(args []string) error {
	fs := newFlagSet("logout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if c.Token != "" {
		cl, err := newAPIClient(c)
		if err == nil {
			if err := cl.call("POST", "/api/agent/logout", true, map[string]string{}, nil); err != nil && !isUnauthorized(err) {
				fmt.Fprintf(os.Stderr, "警告：服务端注销失败（%v），仍会删除本机令牌\n", err)
			}
		}
	}
	c.Token = ""
	c.Name = ""
	c.PairedAt = 0
	if err := saveConfig(c); err != nil {
		return err
	}
	fmt.Println("已退出：本机令牌已删除")
	return nil
}

// ---- 后台服务 ----

func installService(exe string) error {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(exe)
	case "linux":
		return installSystemd(exe)
	case "windows":
		return installSchtasks(exe)
	default:
		return fmt.Errorf("暂不支持 %s 的后台服务安装", runtime.GOOS)
	}
}

func uninstallService() error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd()
	case "linux":
		return uninstallSystemd()
	case "windows":
		return uninstallSchtasks()
	default:
		return fmt.Errorf("暂不支持 %s 的后台服务卸载", runtime.GOOS)
	}
}

func serviceInstalled() (bool, string) {
	switch runtime.GOOS {
	case "darwin":
		path, err := launchdPlistPath()
		if err != nil {
			return false, ""
		}
		_, err = os.Stat(path)
		return err == nil, path
	case "linux":
		path, err := systemdUnitPath()
		if err != nil {
			return false, ""
		}
		_, err = os.Stat(path)
		return err == nil, path
	case "windows":
		err := exec.Command("schtasks", "/Query", "/TN", taskName).Run()
		return err == nil, "计划任务 " + taskName
	default:
		return false, ""
	}
}

const launchdLabel = "ai.stickbox.agent"

func launchdPlist(exe, logPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, xmlEscape(launchdLabel), xmlEscape(exe), xmlEscape(logPath), xmlEscape(logPath))
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func uidString() string {
	out, err := exec.Command("id", "-u").Output()
	if err != nil {
		return strconv.Itoa(os.Getpid()) // 不该发生；只是让错误信息可见
	}
	return strings.TrimSpace(string(out))
}

func installLaunchd(exe string) error {
	plistPath, err := launchdPlistPath()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, "Library", "Logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(logDir, "stickbox-agent.log")

	uid := uidString()
	domain := "gui/" + uid
	// 已加载时先卸掉，否则 bootstrap 会报 already loaded。
	_ = exec.Command("launchctl", "bootout", domain, plistPath).Run()

	if err := os.WriteFile(plistPath, []byte(launchdPlist(exe, logPath)), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput(); err != nil {
		// 老版本 macOS 没有 bootstrap，回退到 load -w。
		if out2, err2 := exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput(); err2 != nil {
			return fmt.Errorf("launchctl 加载失败：%v %s；回退也失败：%v %s",
				err, strings.TrimSpace(string(out)), err2, strings.TrimSpace(string(out2)))
		}
	}
	_ = exec.Command("launchctl", "kickstart", "-k", domain+"/"+launchdLabel).Run()

	fmt.Printf("已安装 launchd 服务：%s\n日志：%s\n", plistPath, logPath)
	return nil
}

func uninstallLaunchd() error {
	plistPath, err := launchdPlistPath()
	if err != nil {
		return err
	}
	domain := "gui/" + uidString()
	if out, err := exec.Command("launchctl", "bootout", domain+"/"+launchdLabel).CombinedOutput(); err != nil {
		// 没加载过也允许卸载。
		_ = exec.Command("launchctl", "unload", "-w", plistPath).Run()
		_ = out
	}
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Printf("已卸载 launchd 服务（%s）\n", plistPath)
	return nil
}

func systemdUnit(exe string) string {
	return fmt.Sprintf(`[Unit]
Description=StickBox usage agent
After=network-online.target

[Service]
Type=simple
ExecStart=%s run
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`, exe)
}

func systemdUnitPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "systemd", "user", "stickbox-agent.service"), nil
}

func installSystemd(exe string) error {
	unitPath, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(systemdUnit(exe)), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload 失败：%v %s（unit 已写入 %s）",
			err, strings.TrimSpace(string(out)), unitPath)
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", "stickbox-agent.service").CombinedOutput(); err != nil {
		return fmt.Errorf("启用 systemd 用户服务失败：%v %s（unit 已写入 %s）",
			err, strings.TrimSpace(string(out)), unitPath)
	}
	fmt.Printf("已安装 systemd 用户服务：%s\n", unitPath)
	return nil
}

func uninstallSystemd() error {
	unitPath, err := systemdUnitPath()
	if err != nil {
		return err
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", "stickbox-agent.service").Run()
	if err := os.Remove(unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Printf("已卸载 systemd 用户服务（%s）\n", unitPath)
	return nil
}

const taskName = "StickBoxAgent"

func installSchtasks(exe string) error {
	tr := fmt.Sprintf(`"%s" run`, exe)
	out, err := exec.Command("schtasks", "/Create", "/SC", "ONLOGON",
		"/TN", taskName, "/TR", tr, "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("创建计划任务失败：%v %s", err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command("schtasks", "/Run", "/TN", taskName).Run()
	fmt.Printf("已安装计划任务：%s\n", taskName)
	return nil
}

func uninstallSchtasks() error {
	out, err := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("删除计划任务失败：%v %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("已卸载计划任务：%s\n", taskName)
	return nil
}

// ---- 入口 ----

func usage() {
	fmt.Print(`stickbox-agent —— StickBox 用量上报代理

用法：
  stickbox-agent pair [--server URL] [--name 名称] [--insecure] <配对码>
  stickbox-agent unpair <dev>
  stickbox-agent devices
  stickbox-agent run [--once]
  stickbox-agent install
  stickbox-agent uninstall
  stickbox-agent status
  stickbox-agent logout

示例：
  stickbox-agent pair 123456
  stickbox-agent run --once
  stickbox-agent install
`)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "pair":
		err = cmdPair(args)
	case "unpair":
		err = cmdUnpair(args)
	case "devices":
		err = cmdDevices(args)
	case "run":
		err = cmdRun(args)
	case "install":
		err = cmdInstall(args)
	case "uninstall":
		err = cmdUninstall(args)
	case "status":
		err = cmdStatus(args)
	case "logout":
		err = cmdLogout(args)
	case "version", "-v", "--version":
		fmt.Println(version)
		return
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "未知命令：%s\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误："+err.Error())
		os.Exit(1)
	}
}
