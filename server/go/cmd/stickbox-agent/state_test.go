package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStatusLinesShowConsecutiveFailuresAndNextRetry(t *testing.T) {
	next := time.Date(2026, 10, 20, 9, 1, 0, 0, time.Local)
	s := agentState{ConsecutiveFailures: 2, NextRetryAt: next.Unix()}
	lines := s.statusLines()
	if len(lines) != 4 {
		t.Fatalf("连续失败时应多一行：%v", lines)
	}
	if !strings.Contains(lines[3], "连续失败：2 轮") || !strings.Contains(lines[3], "09:01:00") {
		t.Errorf("第四行 = %q", lines[3])
	}
	if got := (agentState{}).statusLines(); len(got) != 3 {
		t.Errorf("没有失败时仍是三行：%v", got)
	}
}

func TestRecordScheduleWritesAndClears(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	logf := func(string, ...any) {}
	// 先有一份已有内容的状态，确认不被覆盖。
	if err := saveState(agentState{LastSuccessAt: 7, LastError: "x", LastErrorAt: 8}); err != nil {
		t.Fatal(err)
	}
	next := time.Date(2026, 10, 20, 9, 1, 0, 0, time.UTC)
	recordSchedule(logf, 3, next)
	s := loadState()
	if s.ConsecutiveFailures != 3 || s.NextRetryAt != next.Unix() || s.LastSuccessAt != 7 || s.LastError != "x" {
		t.Fatalf("写入后 = %+v", s)
	}
	recordSchedule(logf, 0, next)
	s = loadState()
	if s.ConsecutiveFailures != 0 || s.NextRetryAt != 0 || s.LastSuccessAt != 7 {
		t.Fatalf("成功后应清零且保留其它字段：%+v", s)
	}
}

// 跳过（limits 为 null）也会写进最近错误，status 才看得见。
func TestSkippedRoundIsRecordedInState(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	recordRoundState(func(string, ...any) {}, &skipError{reason: "limits 为 null，本轮跳过"}, time.Now())
	s := loadState()
	if !strings.Contains(s.LastError, "limits 为 null") || s.ClaudeAuth != "" {
		t.Fatalf("状态 = %+v", s)
	}
}

func TestStateSaveLoadAndPerms(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	if got := loadState(); got != (agentState{}) {
		t.Fatalf("文件不存在应得到空状态：%+v", got)
	}
	want := agentState{LastSuccessAt: 100, LastError: "boom", LastErrorAt: 200, ClaudeAuth: stateAuthOut, ClaudeAuthAt: 300}
	if err := saveState(want); err != nil {
		t.Fatal(err)
	}
	if got := loadState(); got != want {
		t.Fatalf("读回 %+v，期望 %+v", got, want)
	}
	if runtime.GOOS != "windows" {
		p, _ := statePath()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("状态文件权限 = %o，期望 600", perm)
		}
	}
}

func TestLoadStateCorruptIsEmpty(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	p, _ := statePath()
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadState(); got != (agentState{}) {
		t.Fatalf("损坏文件应当空状态：%+v", got)
	}
}

func TestRecordRoundState(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	logf := func(string, ...any) {}
	t0 := time.Unix(1_000, 0)

	recordRoundState(logf, errClaudeNotLoggedIn, t0)
	s := loadState()
	if s.ClaudeAuth != stateAuthOut || !strings.Contains(s.LastError, "claude auth login") || s.LastErrorAt != 1_000 {
		t.Fatalf("未登录失败后：%+v", s)
	}

	// 别的失败不改登录状态，只更新最近错误。
	recordRoundState(logf, errors.New("上报失败"), t0.Add(time.Minute))
	s = loadState()
	if s.ClaudeAuth != stateAuthOut || s.LastError != "上报失败" {
		t.Fatalf("其它失败后：%+v", s)
	}

	// 成功：记成功时间、登录状态转已登录；最近错误保留（历史线索）。
	recordRoundState(logf, nil, t0.Add(2*time.Minute))
	s = loadState()
	if s.LastSuccessAt != 1_120 || s.ClaudeAuth != stateAuthIn || s.LastError != "上报失败" {
		t.Fatalf("成功后：%+v", s)
	}
}

func TestRecordRoundStateTruncatesError(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	recordRoundState(func(string, ...any) {}, errors.New(strings.Repeat("长", 1000)), time.Now())
	if n := len([]rune(loadState().LastError)); n > stateMaxErr+1 {
		t.Errorf("错误文本未截断：%d 字", n)
	}
}

func TestStatusLines(t *testing.T) {
	empty := agentState{}.statusLines()
	if len(empty) != 3 || !strings.Contains(empty[0], "无记录") || !strings.Contains(empty[1], "无") || !strings.Contains(empty[2], "未知") {
		t.Fatalf("空状态：%v", empty)
	}
	s := agentState{LastSuccessAt: 1_000, LastError: "boom", LastErrorAt: 2_000, ClaudeAuth: stateAuthOut, ClaudeAuthAt: 2_000}
	got := strings.Join(s.statusLines(), "\n")
	for _, w := range []string{"上次成功：", "最近错误：boom", "claude auth login"} {
		if !strings.Contains(got, w) {
			t.Errorf("缺少 %q：\n%s", w, got)
		}
	}
}

// runRound 的失败/成功都会写状态；用伪造 claude 走一遍未登录分支。
func TestRunRoundRecordsNotLoggedIn(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	claude := fakeClaudeFiles(t, totalCostStream, `{"loggedIn":false}`+"\n", 1)
	err := runRound(&agentConfig{ClaudePath: claude}, func(string, ...any) {})
	if !errors.Is(err, errClaudeNotLoggedIn) {
		t.Fatalf("err = %v", err)
	}
	s := loadState()
	if s.ClaudeAuth != stateAuthOut || s.LastErrorAt == 0 {
		t.Fatalf("状态未记录：%+v", s)
	}
}

// 正常 usage_report：整轮走通（解析 → 上报 → 记成功），上报内容不受本任务影响。
func TestRunRoundNormalReportsAndRecordsSuccess(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = true
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":1,"devices":1}`))
	}))
	defer srv.Close()

	claude := fakeClaudeFiles(t, readFixture(t, "usage-stream.jsonl"), `{"loggedIn":true}`+"\n", 0)
	cfg := &agentConfig{Server: srv.URL, Token: "tok", ClaudePath: claude}
	if err := runRound(cfg, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if !posted {
		t.Fatal("应当上报")
	}
	s := loadState()
	if s.LastSuccessAt == 0 || s.ClaudeAuth != stateAuthIn {
		t.Fatalf("成功状态未记录：%+v", s)
	}
}
