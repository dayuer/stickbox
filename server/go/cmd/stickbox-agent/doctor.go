package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
)

func (s checkStatus) String() string {
	switch s {
	case statusOK:
		return "OK"
	case statusWarn:
		return "WARN"
	default:
		return "FAIL"
	}
}

type checkResult struct {
	status checkStatus
	title  string
	detail string
	fix    string
}

func okResult(title, detail string) checkResult {
	return checkResult{status: statusOK, title: title, detail: detail}
}

func warnResult(title, detail, fix string) checkResult {
	return checkResult{status: statusWarn, title: title, detail: detail, fix: fix}
}

func failResult(title, detail, fix string) checkResult {
	return checkResult{status: statusFail, title: title, detail: detail, fix: fix}
}

// doctor 的每项检查都做成可注入依赖，测试里用假 claude / httptest 服务端。
type doctor struct {
	cfg *agentConfig
	out io.Writer
	now func() time.Time

	runUsage    func() (usagePayload, string, error)
	apiCall     func(method, path string, out any) error
	lookupHost  func(string) ([]string, error)
	checkHealth func(base string) error
	service     func() (bool, string)
	logPath     string
	tz          func() string
}

func newDoctor(c *agentConfig) *doctor {
	logPath, _ := agentLogPath()
	return &doctor{
		cfg: c,
		out: os.Stdout,
		now: time.Now,
		runUsage: func() (usagePayload, string, error) {
			return collectUsage(c)
		},
		apiCall: func(method, path string, out any) error {
			cl, err := newAPIClient(c)
			if err != nil {
				return err
			}
			return cl.call(method, path, true, nil, out)
		},
		lookupHost:  net.LookupHost,
		checkHealth: healthCheck(c),
		service:     serviceInstalled,
		logPath:     logPath,
		tz:          localTZ,
	}
}

func cmdDoctor(args []string) error {
	fs := newFlagSet("doctor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	return newDoctor(c).run()
}

func (d *doctor) run() error {
	fmt.Fprintf(d.out, "stickbox-agent %s doctor\n\n", version)
	results := []checkResult{
		d.checkClaude(),
		d.checkUsage(),
		d.checkToken(),
		d.checkServer(),
		d.checkService(),
		d.checkTZ(),
	}
	fails := 0
	for _, r := range results {
		fmt.Fprintf(d.out, "[%-4s] %s\n", r.status, r.title)
		if r.detail != "" {
			fmt.Fprintf(d.out, "       %s\n", r.detail)
		}
		if r.status != statusOK && r.fix != "" {
			fmt.Fprintf(d.out, "       建议：%s\n", r.fix)
		}
		if r.status == statusFail {
			fails++
		}
	}
	if fails > 0 {
		return fmt.Errorf("%d 项检查未通过", fails)
	}
	return nil
}

// ---- 1. claude ----

func (d *doctor) checkClaude() checkResult {
	path, err := resolveClaude(d.cfg)
	if err != nil {
		return failResult("claude 可执行文件", err.Error(),
			"安装 Claude Code（https://claude.com/claude-code），确认 `claude --version` 能运行")
	}
	if path == "" {
		return failResult("claude 可执行文件", "没有找到可执行文件",
			"安装 Claude Code，确认 `claude --version` 能运行")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		detail := firstLine(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return warnResult("claude 可执行文件", path+"（版本获取失败："+detail+"）",
			"运行 `claude --version` 看具体报错；必要时重新安装 Claude Code")
	}
	ver := firstNonEmptyLine(string(out))
	if ver == "" {
		ver = "版本未知"
	}
	return okResult("claude 可执行文件", path+"（"+ver+"）")
}

// ---- 2. /usage ----

func (d *doctor) checkUsage() checkResult {
	rep, skip, err := d.runUsage()
	if err != nil {
		detail, fix := classifyUsageError(err)
		return failResult("用量读取", detail, fix)
	}
	if skip != "" {
		detail, fix := classifyUsageSkip(skip)
		return warnResult("用量读取", detail, fix)
	}
	return okResult("用量读取", "五小时 "+fmtWindow(rep.FiveHour)+" / 七天 "+fmtWindow(rep.SevenDay))
}

// classifyUsageError 把常见失败翻译成用户能照着做的中文建议。
func classifyUsageError(err error) (detail, fix string) {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "429") || strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "too many requests"):
		return "Claude 用量接口正在冷却（429）",
			"这是 Claude 侧的临时限流，通常 1 小时内自动恢复；不用改配置，稍后再试"
	case thirdPartyDetector() != "":
		return "claude 使用了第三方端点（" + thirdPartyDetector() + "），读不到官方用量",
			"代理只支持 Claude 官方订阅的用量。取消 ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN 或改回官方登录"
	case strings.Contains(msg, "not logged in") || strings.Contains(msg, "logged out") ||
		strings.Contains(msg, "please log in") || strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "401") || strings.Contains(msg, "authentication"):
		return "Claude Code 未登录：" + firstLine(err.Error()),
			"先在终端运行 `claude` 并完成登录（/login），再运行 stickbox-agent doctor"
	case strings.Contains(msg, "超时"):
		return "claude /usage 执行超时（5 分钟）",
			"检查网络与 Claude 状态；如果经常超时，确认没有安全软件拦截 claude"
	case strings.Contains(msg, "no such file") || strings.Contains(msg, "not found"):
		return "找不到 claude 可执行文件",
			"安装 Claude Code，确认 `claude --version` 能运行"
	default:
		return "读取用量失败：" + firstLine(err.Error()),
			"先手动运行 `claude -p /usage --output-format stream-json --verbose` 看输出；确认已登录且能访问 Claude"
	}
}

func classifyUsageSkip(skip string) (detail, fix string) {
	msg := strings.ToLower(skip)
	switch {
	case strings.Contains(skip, "rate_limits 为 null"):
		return "用量接口暂时不可用（rate_limits 为 null，通常是 429 冷却）",
			"等 1 小时左右再试；这不是本地配置错误"
	case thirdPartyDetector() != "":
		return "没有拿到 rate_limits，检测到第三方端点（" + thirdPartyDetector() + "）",
			"第三方中转/镜像通常不返回官方用量。取消 ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN 或改回官方登录"
	case strings.Contains(msg, "limits 为 null") || strings.Contains(msg, "限额行为空") ||
		strings.Contains(msg, "没有可用的"):
		return skip, "如果正在用第三方中转/镜像，请改回 Claude 官方订阅；否则运行 `claude -p /usage` 确认输出格式"
	default:
		return skip, "手动运行 `claude -p /usage --output-format stream-json --verbose` 检查输出"
	}
}

// thirdPartyDetector 单测可以替换，避免受开发机上的 claude 配置影响。
var thirdPartyDetector = thirdPartyHint

// thirdPartyHint 返回检测到的第三方端点变量名；没有返回空。
// 环境变量和 claude 的用户级 settings.json 都看。
func thirdPartyHint() string {
	for _, k := range []string{
		"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
	} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return k
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return ""
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if strings.TrimSpace(s.Env[k]) != "" {
			return "~/.claude/settings.json 的 " + k
		}
	}
	return ""
}

// ---- 3. 令牌 ----

func (d *doctor) checkToken() checkResult {
	if d.cfg.Token == "" {
		return failResult("配对令牌", "尚未配对",
			"先运行 `stickbox-agent pair <6 位配对码>`（配对码显示在电视/屏幕上）")
	}
	var out struct {
		Devices []deviceRow `json:"devices"`
	}
	if err := d.apiCall("GET", "/api/agent/devices", &out); err != nil {
		if isUnauthorized(err) {
			return failResult("配对令牌", "令牌已失效（服务端返回 401）",
				"重新运行 `stickbox-agent pair <6 位配对码>` 重新绑定")
		}
		return failResult("配对令牌", "无法验证令牌："+firstLine(err.Error()),
			"先看下面的「服务端连通性」；服务端恢复后重新运行 doctor")
	}
	return okResult("配对令牌", fmt.Sprintf("有效（名下 %d 台屏）", len(out.Devices)))
}

// ---- 4. 服务端 ----

func (d *doctor) checkServer() checkResult {
	base, err := normalizeServer(d.cfg.Server)
	if err != nil {
		return failResult("服务端连通性", err.Error(),
			"检查配置里的 server（stickbox-agent status 可看）；对外地址必须是 https://")
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return failResult("服务端连通性", "服务端地址不合法："+base,
			"运行 `stickbox-agent status` 查看配置；重新 pair 时用 --server 指定正确地址")
	}
	host := u.Hostname()
	if _, err := d.lookupHost(host); err != nil {
		return failResult("服务端连通性", "DNS 解析失败："+host,
			"检查网络、代理和 DNS；确认主机名拼写正确（当前 "+host+"）")
	}
	if err := d.checkHealth(base); err != nil {
		detail, fix := classifyServerError(err)
		return failResult("服务端连通性", detail, fix)
	}
	return okResult("服务端连通性", host+"：DNS / TLS / healthz 正常")
}

func classifyServerError(err error) (detail, fix string) {
	var certErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError
	if errors.As(err, &certErr) || errors.As(err, &hostErr) || errors.As(err, &invalidErr) ||
		strings.Contains(err.Error(), "x509") || strings.Contains(err.Error(), "certificate") {
		return "TLS 证书校验失败", "自托管自签证书可在 pair 时加 --insecure（会关闭校验）；公网服务请检查证书链"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such host"):
		return "DNS 解析失败", "检查网络、代理和 DNS"
	case strings.Contains(msg, "connection refused"):
		return "服务端拒绝连接", "确认服务已启动、端口正确、防火墙放行"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
		return "连接服务端超时", "检查网络与代理；确认服务端地址可达"
	case strings.Contains(msg, "503"):
		return "服务端健康检查返回 503（服务在运行但依赖不健康）", "登录服务器查看 stickboxd 日志与 /healthz"
	default:
		return "服务端不可达：" + firstLine(err.Error()),
			"确认地址、网络与服务状态；自托管用户检查容器与证书"
	}
}

func healthCheck(c *agentConfig) func(string) error {
	return func(base string) error {
		cl, err := newAPIClient(c)
		if err != nil {
			return err
		}
		req, err := http.NewRequest("GET", base+"/healthz", nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "stickbox-agent/"+version)
		resp, err := cl.hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("健康检查返回 %d", resp.StatusCode)
		}
		return nil
	}
}

// ---- 5. 后台服务与最近上报 ----

func (d *doctor) checkService() checkResult {
	installed, where := d.service()
	if !installed {
		return warnResult("后台服务", "未安装",
			"先 `stickbox-agent pair <配对码>`，再运行 `stickbox-agent install`")
	}
	last, err := d.lastReport()
	if err != nil {
		return warnResult("后台服务", "已安装（"+where+"），但读不到上报记录："+err.Error(),
			"确认服务已启动；日志位置："+d.logPath+"（macOS 为 launchd，Linux 为 systemd append 日志）")
	}
	return okResult("后台服务", fmt.Sprintf("已安装（%s）；最近一次上报 %s前", where, humanAge(d.now().Sub(last))))
}

func (d *doctor) lastReport() (time.Time, error) {
	if d.logPath == "" {
		return time.Time{}, errors.New("当前系统没有约定的日志文件")
	}
	raw, err := os.ReadFile(d.logPath)
	if err != nil {
		return time.Time{}, err
	}
	lines := strings.Split(string(raw), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.Contains(line, "已上报") {
			continue
		}
		if len(line) < len("2006/01/02 15:04:05") {
			continue
		}
		t, err := time.ParseInLocation("2006/01/02 15:04:05", line[:19], time.Local)
		if err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("日志里还没有成功上报记录")
}

func humanAge(d time.Duration) string {
	if d < time.Minute {
		return "不到 1 分钟"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	}
	return fmt.Sprintf("%d 天", int(d.Hours()/24))
}

// ---- 6. 时区 ----

func (d *doctor) checkTZ() checkResult {
	tz := d.tz()
	if tz == "" {
		return warnResult("时区", "无法确定本机 IANA 时区",
			"设置 TZ=Asia/Shanghai（或你所在时区），或修正 /etc/localtime；否则服务端按默认时区渲染")
	}
	return okResult("时区", tz)
}

// ---- 平台日志位置 ----

// agentLogPath 返回后台服务写入的日志文件，doctor 读它判断最近一次上报。
func agentLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Logs", "stickbox-agent.log"), nil
	case "linux":
		return filepath.Join(home, ".local", "state", "stickbox-agent.log"), nil
	default:
		return "", errors.New("暂不支持读取 " + runtime.GOOS + " 的后台日志")
	}
}

// ---- 小工具 ----

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func firstLine(s string) string {
	line := firstNonEmptyLine(s)
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	return line
}
