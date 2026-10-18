package claudeusage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const happyStream = `{"type":"system","subtype":"init"}
{"type":"assistant","usage_report":{"rate_limits":{"limits":[{"kind":"session","group":"session","percent":2,"resets_at":"2026-10-17T18:40:00.123456+00:00","severity":"normal","is_active":true},{"kind":"weekly_all","group":"weekly","percent":34,"resets_at":"2026-10-22T09:20:00Z","severity":"normal","is_active":false}]}}}
`

func TestParseStream(t *testing.T) {
	rep, skip, err := ParseStream(strings.NewReader(happyStream))
	if err != nil || skip != "" {
		t.Fatalf("err=%v skip=%q", err, skip)
	}
	if rep.FiveHour == nil || rep.FiveHour.UsedPercentage != 2 ||
		rep.SevenDay == nil || rep.SevenDay.UsedPercentage != 34 {
		t.Fatalf("report = %+v", rep)
	}
	want := time.Date(2026, 10, 17, 18, 40, 0, 123456000, time.UTC).Unix()
	if rep.FiveHour.ResetsAt != want {
		t.Fatalf("resets_at = %d，期望 %d", rep.FiveHour.ResetsAt, want)
	}
	if len(rep.Limits) == 0 {
		t.Fatal("原始 limits 丢了")
	}
}

func TestParseStreamSkipReasons(t *testing.T) {
	cases := []string{
		`{"type":"assistant","usage_report":{"rate_limits":null}}`,
		`{"type":"assistant","usage_report":{"rate_limits":{"limits":null}}}`,
		`{"type":"assistant","usage_report":{"rate_limits":{"limits":[]}}}`,
	}
	for _, in := range cases {
		_, skip, err := ParseStream(strings.NewReader(in))
		if err != nil || skip == "" {
			t.Errorf("%s → err=%v skip=%q", in, err, skip)
		}
	}
	if _, _, err := ParseStream(strings.NewReader(`{"type":"system"}`)); err == nil {
		t.Fatal("没有 usage_report 应报错")
	}
}

func TestParseResetsAt(t *testing.T) {
	if got, err := ParseResetsAt("2026-10-22T09:20:00+08:00"); err != nil ||
		got != time.Date(2026, 10, 22, 1, 20, 0, 0, time.UTC).Unix() {
		t.Fatalf("got=%d err=%v", got, err)
	}
	if _, err := ParseResetsAt("not-a-time"); err == nil {
		t.Fatal("非法时间应报错")
	}
}

func TestFilterEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin", "HOME=/Users/example", "LC_ALL=zh_CN.UTF-8",
		"https_proxy=http://proxy:8080", "NODE_EXTRA_CA_CERTS=/etc/ssl/x.pem",
		"ANTHROPIC_AUTH_TOKEN=x", "CLAUDE_CODE_ENTRYPOINT=cli",
		"NODE_OPTIONS=--require=/tmp/evil.js", "LD_PRELOAD=/tmp/evil.dylib",
	}
	got := FilterEnv(in)
	want := in[:5]
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got=%v want=%v", got, want)
	}
	if EnvNameAllowed("windows", "USERPROFILE") != true ||
		EnvNameAllowed("darwin", "USERPROFILE") != false {
		t.Fatal("windows 白名单不对")
	}
}

func TestWorkDirPrivate(t *testing.T) {
	base := t.TempDir()
	work, err := WorkDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(work) != base || filepath.Base(work) != "work" {
		t.Fatalf("work = %s", work)
	}
	st, err := os.Stat(work)
	if err != nil || !st.IsDir() {
		t.Fatalf("stat = %v err=%v", st, err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o700 {
		t.Fatalf("perm = %o", st.Mode().Perm())
	}
	if _, err := WorkDir(""); err == nil {
		t.Fatal("空 base 应报错")
	}
}

func TestTailTextAndExecError(t *testing.T) {
	if got := TailText([]byte("abcdef"), 3); got != "def" {
		t.Fatalf("TailText = %q", got)
	}
	// 截断到非法 UTF-8 时替换掉，而不是原样返回坏字节。
	if got := TailText([]byte("ab\xe4\xb8"), 3); strings.ContainsRune(got, '�') == false {
		t.Fatalf("TailText 没处理坏 UTF-8：%q", got)
	}
	err := ExecError(errors.New("exit status 1"), false, time.Second, "boom")
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("ExecError = %v", err)
	}
	err = ExecError(errors.New("killed"), true, 2*time.Second, "")
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("超时错误 = %v", err)
	}
}
