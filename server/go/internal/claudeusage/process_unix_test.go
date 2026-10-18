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

	// 机器负载高时 shell 启动可能被推迟，给足余量；断言语义不变：
	// Run 必须在超时后合理时间内返回，且孙进程确实被回收。
	const timeout = 5 * time.Second
	start := time.Now()
	_, _, err := Run(context.Background(), Config{
		ClaudePath: script,
		WorkDir:    dir,
		Timeout:    timeout,
		Env:        os.Environ(),
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("应超时")
	}
	var rf *RunFailure
	if !errors.As(err, &rf) || !rf.TimedOut {
		t.Fatalf("err = %v（耗时 %s）", err, elapsed)
	}
	// 上限只用于抓「Wait 被残留管道拖死」；正常情况下应接近 timeout。
	if limit := timeout + 20*time.Second; elapsed > limit {
		t.Fatalf("超时后 %s 才返回（上限 %s），Wait 被残留进程拖住了", elapsed, limit)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("读不到孙进程 pid（脚本没跑到，耗时 %s）：%v", elapsed, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// SIGKILL 后要等内核回收；高负载下回收也可能变慢。
	deadline := time.Now().Add(10 * time.Second)
	for {
		if syscall.Kill(pid, 0) != nil {
			return // ESRCH：已经没了
		}
		if time.Now().After(deadline) {
			t.Fatalf("孙进程 %d 在 %s 后仍活着，进程组没杀干净", pid, time.Since(start))
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
