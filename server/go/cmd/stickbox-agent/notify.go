package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// 连续失败多少轮才打扰用户（10 分钟一轮，约 30 分钟）。
	notifyAfterFailures = 3
	// 同一原因两次通知的最短间隔。
	notifyCooldown = time.Hour
	notifyTimeout  = 5 * time.Second
)

// notifier 负责把消息弹给用户；失败只记日志，不影响上报。
type notifier func(title, body string) error

// failureTracker 统计连续失败轮数，到阈值后按「原因」限频通知。
type failureTracker struct {
	mu       sync.Mutex
	fails    int
	notified map[string]time.Time // 原因 → 上次通知时间
	notify   notifier
	logf     func(string, ...any)
	now      func() time.Time
}

func newFailureTracker(notify notifier, logf func(string, ...any)) *failureTracker {
	return &failureTracker{
		notified: map[string]time.Time{},
		notify:   notify,
		logf:     logf,
		now:      time.Now,
	}
}

// record 记录一轮结果。nil 清零计数；errNotPaired 是用户还没配对，
// 不算故障，既不累计也不清零。
func (t *failureTracker) record(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err == nil {
		t.fails = 0
		return
	}
	if errors.Is(err, errNotPaired) {
		return
	}
	t.fails++
	if t.fails < notifyAfterFailures {
		return
	}
	now := t.now()
	reason := err.Error()
	// 清掉过期条目：错误文本里可能带变化的 stderr，别让 map 无限长。
	for k, at := range t.notified {
		if now.Sub(at) >= notifyCooldown {
			delete(t.notified, k)
		}
	}
	if _, ok := t.notified[reason]; ok {
		return
	}
	t.notified[reason] = now
	body := fmt.Sprintf("已连续 %d 轮失败：%s", t.fails, truncateRunes(reason, 160))
	if nerr := t.notify("StickBox 用量上报失败", body); nerr != nil {
		t.logf("系统通知未发出：%v", nerr)
	}
}

// withFailureNotify 包装一轮：执行后把结果交给 tracker。
func withFailureNotify(round func() error, t *failureTracker) func() error {
	return func() error {
		err := round()
		t.record(err)
		return err
	}
}

// systemNotify 按平台发系统通知：macOS 用 osascript，Linux 用 notify-send
// （没装就返回错误由调用方记日志），Windows 不发。
func systemNotify(title, body string) error {
	return systemNotifyOS(runtime.GOOS, exec.LookPath, runCmd, title, body)
}

func runCmd(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run()
}

func systemNotifyOS(
	goos string,
	lookPath func(string) (string, error),
	run func(name string, args ...string) error,
	title, body string,
) error {
	switch goos {
	case "darwin":
		script := fmt.Sprintf("display notification %s with title %s",
			appleScriptString(body), appleScriptString(title))
		return run("osascript", "-e", script)
	case "linux":
		p, err := lookPath("notify-send")
		if err != nil {
			return errors.New("没有 notify-send，只记日志")
		}
		return run(p, title, body)
	}
	return fmt.Errorf("%s 不发系统通知，只记日志", goos)
}

// appleScriptString 生成带引号的 AppleScript 字符串字面量。
// 换行折成空格，反斜杠和双引号转义，防止错误文本（可能含 stderr）注入脚本。
func appleScriptString(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
	return `"` + s + `"`
}
