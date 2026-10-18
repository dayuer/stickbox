//go:build !windows

package claudeusage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 超时必须杀掉整个进程组：只杀直接子进程的话，孙进程会变成孤儿。
func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "claude-fake")
	body := "#!/bin/sh\nsleep 60 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, _, err := Run(context.Background(), Config{
		ClaudePath: script,
		WorkDir:    dir,
		Timeout:    2 * time.Second,
		Env:        os.Environ(),
	})
	if err == nil {
		t.Fatal("应超时")
	}
	var rf *RunFailure
	if !errors.As(err, &rf) || !rf.TimedOut {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("超时后 %s 才返回，Wait 被残留进程拖住了", d)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("读不到孙进程 pid（脚本没跑到）：%v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// SIGKILL 后要等内核回收，最多等 2 秒确认它已经不在了。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if syscall.Kill(pid, 0) != nil {
			return // ESRCH：已经没了
		}
		if time.Now().After(deadline) {
			t.Fatalf("孙进程 %d 还活着，进程组没杀干净", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// OnExit 要给出内核记账的峰值 RSS 和退出码。
func TestRunProcessStats(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude-fake")
	body := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"assistant\",\"usage_report\":{\"rate_limits\":{\"limits\":[{\"kind\":\"session\",\"percent\":1,\"resets_at\":\"2026-10-18T23:59:00Z\"}]}}}'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	var stats ProcessStats
	out, _, err := Run(context.Background(), Config{
		ClaudePath: script,
		WorkDir:    dir,
		Timeout:    5 * time.Second,
		Env:        os.Environ(),
		OnExit:     func(s ProcessStats) { stats = s },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ExitCode != 0 {
		t.Fatalf("ExitCode = %d", stats.ExitCode)
	}
	if stats.PeakRSSBytes <= 0 {
		t.Fatalf("PeakRSSBytes = %d", stats.PeakRSSBytes)
	}
	if _, skip, err := ParseStream(strings.NewReader(string(out))); err != nil || skip != "" {
		t.Fatalf("stdout 没写全：skip=%q err=%v", skip, err)
	}
}
