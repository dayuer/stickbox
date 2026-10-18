//go:build windows

package claudeusage

import (
	"os"
	"os/exec"
)

func configureProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func processPeakRSS(ps *os.ProcessState) int64 { return 0 }
