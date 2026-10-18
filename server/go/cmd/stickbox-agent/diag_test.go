package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeClaudeFiles 写一个伪造的 claude 可执行文件（POSIX shell）。
// agent 会过滤环境变量，所以输出内容放在脚本同目录的文件里：
//
//	usage.out  —— `claude -p /usage ...` 的 stdout
//	auth.out   —— `claude auth status` 的 stdout（authExit 为其退出码）
//	cwd.txt    —— 记录被调用时的工作目录，用来核对专用空目录约定
func fakeClaudeFiles(t *testing.T, usageOut, authOut string, authExit int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("伪造 claude 用 POSIX shell")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("usage.out", usageOut)
	write("auth.out", authOut)
	script := "#!/bin/sh\n" +
		"d=$(dirname \"$0\")\n" +
		"pwd > \"$d/cwd.txt\"\n" +
		"if [ \"$1\" = auth ]; then cat \"$d/auth.out\"; exit " + string(rune('0'+authExit)) + "; fi\n" +
		"cat \"$d/usage.out\"\n"
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

const totalCostStream = `{"type":"system","subtype":"init","session_id":"x"}
{"type":"result","subtype":"success","is_error":false,"result":"Total cost:            $0.0000\nTotal duration (API):  0s"}
`

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 三个分支之一：Total cost + loggedIn:false → 明确的「未登录」提示。
func TestCollectUsageNotLoggedIn(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("STICKBOX_CONFIG_DIR", cfgDir)
	claude := fakeClaudeFiles(t, totalCostStream, `{"loggedIn":false}`+"\n", 1) // 真实 claude 未登录时可能非 0 退出

	_, _, err := collectUsage(&agentConfig{ClaudePath: claude})
	if !errors.Is(err, errClaudeNotLoggedIn) {
		t.Fatalf("err = %v，期望 errClaudeNotLoggedIn", err)
	}
	if !strings.Contains(err.Error(), "claude auth login") {
		t.Errorf("提示里应包含 claude auth login：%v", err)
	}
	// 沿用专用空目录约定：claude 的工作目录是 <配置目录>/work。
	cwd, _ := os.ReadFile(filepath.Join(filepath.Dir(claude), "cwd.txt"))
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(cwd)))
	want, _ := filepath.EvalSymlinks(filepath.Join(cfgDir, "work"))
	if got != want {
		t.Errorf("claude 工作目录 = %q，期望 %q", got, want)
	}
}

// 三个分支之二：Total cost 但 auth status 说已登录 → 保留原兜底提示。
func TestCollectUsageTotalCostButLoggedIn(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	claude := fakeClaudeFiles(t, totalCostStream, `{"loggedIn":true}`+"\n", 0)

	_, _, err := collectUsage(&agentConfig{ClaudePath: claude})
	var nr *noReportError
	if !errors.As(err, &nr) || !strings.Contains(err.Error(), "格式可能变了") {
		t.Fatalf("err = %v，期望兜底的 noReportError", err)
	}
}

// auth status 本身查不出来（非 JSON）时也走兜底，不误报未登录。
func TestCollectUsageAuthStatusUnparseable(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	claude := fakeClaudeFiles(t, totalCostStream, "oops\n", 2)

	_, _, err := collectUsage(&agentConfig{ClaudePath: claude})
	var nr *noReportError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v，期望兜底的 noReportError", err)
	}
}

// 没有 Total cost 的缺 usage_report 输出不该去查 auth status：
// 让 auth status 输出 loggedIn:false，结果仍应是兜底提示。
func TestCollectUsageNoReportWithoutTotalCostSkipsAuthCheck(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	claude := fakeClaudeFiles(t, readFixture(t, "usage-no-report.jsonl"), `{"loggedIn":false}`+"\n", 0)

	_, _, err := collectUsage(&agentConfig{ClaudePath: claude})
	if errors.Is(err, errClaudeNotLoggedIn) {
		t.Fatal("没有 Total cost 特征时不应判成未登录")
	}
	var nr *noReportError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v，期望 noReportError", err)
	}
}

// 三个分支之三：正常 usage_report 照常解析。
func TestCollectUsageNormal(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	claude := fakeClaudeFiles(t, readFixture(t, "usage-stream.jsonl"), `{"loggedIn":true}`+"\n", 0)

	rep, skip, err := collectUsage(&agentConfig{ClaudePath: claude})
	if err != nil || skip != "" {
		t.Fatalf("err=%v skip=%q", err, skip)
	}
	if rep.FiveHour == nil || rep.SevenDay == nil {
		t.Fatalf("应解析出两个窗口：%+v", rep)
	}
}

func TestParseAuthStatus(t *testing.T) {
	cases := map[string]authState{
		`{"loggedIn":false}`:                 authLoggedOut,
		`{"loggedIn":true,"authMethod":"x"}`: authLoggedIn,
		`{}`:                                 authUnknown,
		``:                                   authUnknown,
		`not json`:                           authUnknown,
		`{"loggedIn":"no"}`:                  authUnknown,
	}
	for in, want := range cases {
		if got := parseAuthStatus([]byte(in)); got != want {
			t.Errorf("parseAuthStatus(%q) = %v，期望 %v", in, got, want)
		}
	}
}
