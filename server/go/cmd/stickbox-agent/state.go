package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// agentState 是 `stickbox-agent status` 要展示的运行状态，和 agent.json
// （配置/令牌）分开存：状态每轮都会写，配置只在配对时写。
type agentState struct {
	LastSuccessAt int64  `json:"last_success_at,omitempty"` // Unix 秒
	LastError     string `json:"last_error,omitempty"`
	LastErrorAt   int64  `json:"last_error_at,omitempty"`
	// ClaudeAuth 是最近一次确认到的 claude 登录状态：logged_in / logged_out / 空=未知。
	ClaudeAuth   string `json:"claude_auth,omitempty"`
	ClaudeAuthAt int64  `json:"claude_auth_at,omitempty"`
	// ConsecutiveFailures 是连续失败轮数（含快速重试；成功清零；未配对不算）。
	// NextRetryAt 是 agent 计划的下一次尝试时间（Unix 秒），仅在连续失败时有值。
	ConsecutiveFailures int   `json:"consecutive_failures,omitempty"`
	NextRetryAt         int64 `json:"next_retry_at,omitempty"`
}

const (
	stateAuthIn  = "logged_in"
	stateAuthOut = "logged_out"
	stateMaxErr  = 300 // 错误文本可能带 stderr，截断后再落盘
)

func statePath() (string, error) {
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "state.json"), nil
}

// loadState 读状态文件。文件不存在或损坏都当作空状态：
// 状态只是展示用，不该因它出问题挡住上报。
func loadState() agentState {
	var s agentState
	p, err := statePath()
	if err != nil {
		return s
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return s
	}
	if json.Unmarshal(b, &s) != nil {
		return agentState{}
	}
	return s
}

// saveState 原子写（临时文件 + rename），权限 0600。
func saveState(s agentState) error {
	p, err := statePath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败：%w", err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // rename 成功后这里是无害的 ENOENT
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// recordRoundState 把一轮结果写进状态文件。err==nil 表示成功上报
// （也说明 claude 已登录）。写失败只记日志。
func recordRoundState(logf func(string, ...any), err error, now time.Time) {
	s := loadState()
	ts := now.Unix()
	switch {
	case err == nil:
		s.LastSuccessAt = ts
		s.ClaudeAuth, s.ClaudeAuthAt = stateAuthIn, ts
	default:
		s.LastError = truncateRunes(err.Error(), stateMaxErr)
		s.LastErrorAt = ts
		if errors.Is(err, errClaudeNotLoggedIn) {
			s.ClaudeAuth, s.ClaudeAuthAt = stateAuthOut, ts
		}
	}
	if werr := saveState(s); werr != nil {
		logf("写状态文件失败（不影响上报）：%v", werr)
	}
}

// recordSchedule 在每轮结束后记下连续失败轮数和下次尝试时间。
// fails==0（成功）时两项都清掉。写失败只记日志。
func recordSchedule(logf func(string, ...any), fails int, next time.Time) {
	s := loadState()
	if fails <= 0 {
		s.ConsecutiveFailures, s.NextRetryAt = 0, 0
	} else {
		s.ConsecutiveFailures, s.NextRetryAt = fails, next.Unix()
	}
	if werr := saveState(s); werr != nil {
		logf("写状态文件失败（不影响上报）：%v", werr)
	}
}

func fmtStateTime(ts int64) string {
	return time.Unix(ts, 0).Local().Format("2006-01-02 15:04")
}

// statusLines 生成 status 命令的三行；连续失败时多一行。
func (s agentState) statusLines() []string {
	last := "上次成功：无记录"
	if s.LastSuccessAt > 0 {
		last = "上次成功：" + fmtStateTime(s.LastSuccessAt)
	}
	errLine := "最近错误：无"
	if s.LastError != "" {
		errLine = fmt.Sprintf("最近错误：%s（%s）", s.LastError, fmtStateTime(s.LastErrorAt))
	}
	auth := "claude 登录：未知（还没确认过）"
	at := ""
	if s.ClaudeAuthAt > 0 {
		at = "（" + fmtStateTime(s.ClaudeAuthAt) + " 确认）"
	}
	switch s.ClaudeAuth {
	case stateAuthIn:
		auth = "claude 登录：已登录" + at
	case stateAuthOut:
		auth = "claude 登录：未登录，请在终端执行 `claude auth login`" + at
	}
	lines := []string{last, errLine, auth}
	if s.ConsecutiveFailures > 0 {
		retry := fmt.Sprintf("连续失败：%d 轮", s.ConsecutiveFailures)
		if s.NextRetryAt > 0 {
			retry += "，下次尝试：" + time.Unix(s.NextRetryAt, 0).Local().Format("2006-01-02 15:04:05")
		}
		lines = append(lines, retry)
	}
	return lines
}
