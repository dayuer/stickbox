//go:build !windows

package claudeusage

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 杀掉以 pid 为组长的整个进程组。组已不存在时忽略。
func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// processPeakRSS 返回内核记账的子进程峰值 RSS（字节）。
// Linux 的 ru_maxrss 单位是 KB，macOS 是字节。
func processPeakRSS(ps *os.ProcessState) int64 {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0
	}
	if runtime.GOOS == "linux" {
		return ru.Maxrss * 1024
	}
	return ru.Maxrss
}
