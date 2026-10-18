// Package claudeusage 采集本机 `claude -p /usage` 的用量。
//
// 这是从 cmd/stickbox-agent 原样抽出的逻辑（L-E0）：白名单环境 + 专用空
// 工作目录 + stream-json 解析。托管模式的 agent 与本地模式共用这一份，
// 不再各写一套。
package claudeusage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/usage"
)

// 与服务端 internal/usage 的线格式一致。
type (
	Window = usage.Window
	Report = usage.Report
)

type usageReportWire struct {
	RateLimits *rateLimitsWire `json:"rate_limits"`
}

type rateLimitsWire struct {
	Limits json.RawMessage `json:"limits"`
}

// limitRow 是 /usage 的 rate_limits.limits[] 一行。
type limitRow struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	ResetsAt *string `json:"resets_at"`
	Severity string  `json:"severity"`
	IsActive bool    `json:"is_active"`
}

func (r limitRow) window() (*Window, error) {
	if r.ResetsAt == nil || strings.TrimSpace(*r.ResetsAt) == "" {
		return nil, errors.New("resets_at 缺失")
	}
	ts, err := ParseResetsAt(*r.ResetsAt)
	if err != nil {
		return nil, err
	}
	return &Window{UsedPercentage: r.Percent, ResetsAt: ts}, nil
}

// ParseStream 从 `claude ... --output-format stream-json` 的逐行输出里
// 取最后一个带 usage_report 的事件。
//
// skipReason 非空表示「本轮不报」——冷却期、没有限额行等，都不是错误。
func ParseStream(r io.Reader) (Report, string, error) {
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
		return Report{}, "", fmt.Errorf("读取 claude 输出失败：%w", err)
	}
	if report == nil {
		return Report{}, "", errors.New("claude 输出里没有 usage_report（/usage 的输出格式可能变了）")
	}
	if report.RateLimits == nil {
		return Report{}, "用量接口暂时不可用（rate_limits 为 null，通常是刚触发过 429 冷却），本轮跳过", nil
	}
	return mapRateLimits(report.RateLimits.Limits)
}

func mapRateLimits(raw json.RawMessage) (Report, string, error) {
	var rep Report
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

// ParseResetsAt 解析 ISO 8601；RFC3339Nano 同时接受小数秒、Z 和 +00:00。
func ParseResetsAt(s string) (int64, error) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("无法解析 resets_at %q：%w", s, err)
	}
	return t.Unix(), nil
}

// FilterEnv 只透传白名单变量。
//
// 黑名单挡不住：Claude 桌面版会往子进程环境里塞其他变量，实测只剔除
// ANTHROPIC_* / CLAUDE_* 时 /usage 仍返回 rate_limits=null。
func FilterEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if EnvNameAllowed(runtime.GOOS, name) {
			out = append(out, kv)
		}
	}
	return out
}

func EnvNameAllowed(goos, name string) bool {
	up := strings.ToUpper(name)
	if strings.HasPrefix(up, "LC_") {
		return true
	}
	switch up {
	case "HOME", "USER", "LOGNAME", "PATH", "SHELL", "TMPDIR", "TERM", "LANG",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", // 大小写两种写法都靠 ToUpper 命中
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS":
		return true
	}
	if goos == "windows" {
		switch up {
		case "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SYSTEMROOT", "COMSPEC",
			"PATHEXT", "USERNAME", "TEMP", "TMP":
			return true
		}
	}
	return false
}

// WorkDir 返回 claude 的专用工作目录：baseDir 下的空目录。
//
// 不能用 HOME：在 HOME 下 --setting-sources project 读到的「项目设置」
// 就是 ~/.claude/settings.json，也就是用户的全局设置——正是要避开的。
func WorkDir(baseDir string) (string, error) {
	if strings.TrimSpace(baseDir) == "" {
		return "", errors.New("缺少配置目录")
	}
	work := filepath.Join(baseDir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return "", fmt.Errorf("创建 claude 工作目录失败：%w", err)
	}
	_ = os.Chmod(work, 0o700) // Windows 上忽略
	return work, nil
}

// Config 是执行一次 claude 的输入。
type Config struct {
	ClaudePath string        // 已解析的 claude 可执行文件
	WorkDir    string        // 专用空目录（WorkDir 的返回值）
	Timeout    time.Duration // 0 表示不额外加超时
	Env        []string      // 原始环境；内部按白名单过滤
	// OnStart 在进程启动后调用，可用来采样峰值内存。
	OnStart func(pid int)
}

// Run 执行一次 `claude -p /usage`，返回原始 stdout 和 stderr 末尾。
func Run(ctx context.Context, cfg Config) (stdout []byte, stderrTail string, err error) {
	if cfg.ClaudePath == "" {
		return nil, "", errors.New("没有 claude 可执行文件")
	}
	if cfg.WorkDir == "" {
		return nil, "", errors.New("没有 claude 工作目录")
	}
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, cfg.ClaudePath,
		"-p", "/usage",
		"--setting-sources", "project",
		"--no-session-persistence",
		"--output-format", "stream-json",
		"--verbose",
	)
	cmd.Env = FilterEnv(cfg.Env)
	cmd.Dir = cfg.WorkDir

	// 显式接 /dev/null：launchd 下继承的 stdin 可能是个不关闭的管道。
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, "", fmt.Errorf("打开 %s 失败：%w", os.DevNull, err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull

	stderr := &tailWriter{max: 64 << 10}
	cmd.Stderr = stderr
	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("启动 claude 失败：%w", err)
	}
	if cfg.OnStart != nil {
		cfg.OnStart(cmd.Process.Pid)
	}
	waitErr := cmd.Wait()
	tail := TailText(stderr.buf, 600)
	if waitErr != nil {
		return out.Bytes(), tail, ExecError(waitErr, errors.Is(ctx.Err(), context.DeadlineExceeded),
			cfg.Timeout, tail)
	}
	return out.Bytes(), tail, nil
}

// Collect 是 Run + ParseStream。
func Collect(ctx context.Context, cfg Config) (Report, string, error) {
	out, tail, err := Run(ctx, cfg)
	if err != nil {
		return Report{}, "", err
	}
	_ = tail
	return ParseStream(bytes.NewReader(out))
}

// ExecError 把 claude 的退出错误变成可读的中文错误。
func ExecError(err error, timedOut bool, timeout time.Duration, stderrTail string) error {
	suffix := ""
	if stderrTail != "" {
		suffix = "；stderr 末尾：" + stderrTail
	}
	if timedOut {
		return fmt.Errorf("claude 执行超时（%s）%s", timeout, suffix)
	}
	return fmt.Errorf("claude 执行失败：%v%s", err, suffix)
}

// TailWriter 只保留最后 max 字节，避免异常输出把内存吃光。
type tailWriter struct {
	buf []byte
	max int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

// TailText 取末尾 n 个字节，并保证是合法 UTF-8 边界附近的可读文本。
func TailText(b []byte, n int) string {
	if n <= 0 || len(b) == 0 {
		return ""
	}
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.ToValidUTF8(strings.TrimSpace(string(b)), "�")
}
