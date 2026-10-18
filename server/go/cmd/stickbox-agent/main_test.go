package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// 真实 claude 输出的脱敏样例：session → five_hour，weekly_all → seven_day。
func TestParseUsageStreamMapsSessionAndWeekly(t *testing.T) {
	rep, skip, err := parseUsageStream(fixture(t, "usage-stream.jsonl"))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if skip != "" {
		t.Fatalf("不该跳过：%s", skip)
	}
	if rep.FiveHour == nil || rep.SevenDay == nil {
		t.Fatalf("窗口缺失：%+v", rep)
	}
	if rep.FiveHour.UsedPercentage != 2 {
		t.Errorf("five_hour = %v，期望 2", rep.FiveHour.UsedPercentage)
	}
	if rep.SevenDay.UsedPercentage != 34 {
		t.Errorf("seven_day = %v，期望 34", rep.SevenDay.UsedPercentage)
	}
	// 小数秒和 +00:00 都要能解析。
	want := time.Date(2026, 10, 17, 18, 40, 0, 123456000, time.UTC).Unix()
	if rep.FiveHour.ResetsAt != want {
		t.Errorf("five_hour.resets_at = %d，期望 %d", rep.FiveHour.ResetsAt, want)
	}
	want7 := time.Date(2026, 10, 22, 9, 20, 0, 0, time.UTC).Unix()
	if rep.SevenDay.ResetsAt != want7 {
		t.Errorf("seven_day.resets_at = %d，期望 %d", rep.SevenDay.ResetsAt, want7)
	}
	// 原始 limits 要原样带上（服务端存下来给 App 看）。
	var rows []map[string]any
	if err := json.Unmarshal(rep.Limits, &rows); err != nil {
		t.Fatalf("原始 limits 不是数组：%v", err)
	}
	if len(rows) != 2 || rows[0]["kind"] != "session" || rows[1]["kind"] != "weekly_all" {
		t.Errorf("原始 limits = %s", rep.Limits)
	}
}

// 429 冷却期内 rate_limits 为 null：跳过本轮，不报错。
func TestParseUsageStreamSkipsCooling(t *testing.T) {
	rep, skip, err := parseUsageStream(fixture(t, "usage-cooling.jsonl"))
	if err != nil {
		t.Fatalf("冷却不该报错：%v", err)
	}
	if skip == "" {
		t.Fatal("rate_limits 为 null 时应该跳过")
	}
	if rep.FiveHour != nil || rep.SevenDay != nil {
		t.Errorf("冷却期不该有窗口：%+v", rep)
	}
}

// limits 为 null（服务端没回行）同样跳过。
func TestParseUsageStreamSkipsNullLimits(t *testing.T) {
	_, skip, err := parseUsageStream(fixture(t, "usage-no-limits.jsonl"))
	if err != nil {
		t.Fatalf("limits 为 null 不该报错：%v", err)
	}
	if skip == "" {
		t.Fatal("limits 为 null 时应该跳过")
	}
}

// 没有 usage_report 说明输出格式变了，必须报错而不是静默。
func TestParseUsageStreamErrorsWithoutReport(t *testing.T) {
	_, _, err := parseUsageStream(fixture(t, "usage-no-report.jsonl"))
	if err == nil {
		t.Fatal("缺少 usage_report 时应报错")
	}
}

func TestParseUsageStreamIgnoresUnknownRows(t *testing.T) {
	in := strings.NewReader(`{"type":"assistant","usage_report":{"rate_limits":{"limits":[` +
		`{"kind":"weekly_scoped","group":"weekly","percent":5,"resets_at":"2026-10-22T09:20:00Z","severity":"normal","is_active":false},` +
		`{"kind":"session","group":"session","percent":10,"resets_at":"2026-10-17T18:40:00Z","severity":"normal","is_active":true}]}}}`)
	rep, skip, err := parseUsageStream(in)
	if err != nil || skip != "" {
		t.Fatalf("err=%v skip=%q", err, skip)
	}
	if rep.FiveHour == nil || rep.FiveHour.UsedPercentage != 10 {
		t.Errorf("five_hour = %+v", rep.FiveHour)
	}
	if rep.SevenDay != nil {
		t.Errorf("weekly_scoped 不该映射到 seven_day：%+v", rep.SevenDay)
	}
}

func TestParseResetsAt(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		bad  bool
	}{
		{"2026-10-17T18:40:00.123456+00:00", time.Date(2026, 10, 17, 18, 40, 0, 123456000, time.UTC).Unix(), false},
		{"2026-10-22T09:20:00Z", time.Date(2026, 10, 22, 9, 20, 0, 0, time.UTC).Unix(), false},
		{"2026-10-22T09:20:00+08:00", time.Date(2026, 10, 22, 1, 20, 0, 0, time.UTC).Unix(), false},
		{"not-a-time", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := parseResetsAt(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q 应该报错", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q = %d, %v；期望 %d", c.in, got, err, c.want)
		}
	}
}

func TestLocalTZPrefersEnv(t *testing.T) {
	t.Setenv("TZ", "Asia/Bangkok")
	if got := localTZ(); got != "Asia/Bangkok" {
		t.Errorf("localTZ = %q，期望 Asia/Bangkok", got)
	}
	t.Setenv("TZ", "posix/Asia/Shanghai")
	if got := localTZ(); got != "Asia/Shanghai" {
		t.Errorf("posix 前缀没去掉：%q", got)
	}
	t.Setenv("TZ", "right/Asia/Shanghai")
	if got := localTZ(); got != "Asia/Shanghai" {
		t.Errorf("right 前缀没去掉：%q", got)
	}
	t.Setenv("TZ", "Not/AZone")
	if got := localTZ(); got == "Not/AZone" {
		t.Errorf("非法 TZ 被原样上报：%q", got)
	}
}

// 子进程环境必须走白名单：黑名单挡不住 Claude 桌面版注入的其他变量。
func TestFilterEnvKeepsOnlyWhitelist(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"HOME=/Users/example",
		"USER=example",
		"LOGNAME=example",
		"SHELL=/bin/zsh",
		"TMPDIR=/var/folders/x",
		"LANG=zh_CN.UTF-8",
		"LC_ALL=zh_CN.UTF-8",
		"LC_CTYPE=UTF-8",
		"TERM=xterm-256color",
		"HTTP_PROXY=http://proxy:8080",
		"HTTPS_PROXY=http://proxy:8080",
		"NO_PROXY=localhost",
		"http_proxy=http://proxy:8080",
		"https_proxy=http://proxy:8080",
		"no_proxy=localhost",
		"SSL_CERT_FILE=/etc/ssl/cert.pem",
		"SSL_CERT_DIR=/etc/ssl/certs",
		"NODE_EXTRA_CA_CERTS=/etc/ssl/extra.pem",
		// 以下都必须剔掉
		"ANTHROPIC_AUTH_TOKEN=x",
		"ANTHROPIC_BASE_URL=https://example",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CODE_SSE_PORT=1234",
		"CLAUDECODE=1",
		"CLAUDE_PID=42",
		"NODE_OPTIONS=--require=/tmp/evil.js",
		"LD_PRELOAD=/tmp/evil.dylib",
		"DYLD_INSERT_LIBRARIES=/tmp/evil.dylib",
		"OPENAI_API_KEY=x",
		"SOMETHING_ELSE=1",
	}
	got := filterEnv(in)
	want := in[:19] // 前 19 个是白名单
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("白名单过滤结果 = %v\n期望 %v", got, want)
	}
}

// Windows 的家庭目录与系统变量名和 Unix 不同。
func TestEnvNameAllowedWindows(t *testing.T) {
	for _, name := range []string{"USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot", "ComSpec", "PATHEXT", "TEMP", "TMP"} {
		if !envNameAllowed("windows", name) {
			t.Errorf("windows 下应保留 %s", name)
		}
		if envNameAllowed("darwin", name) {
			t.Errorf("darwin 下不该保留 %s", name)
		}
	}
	if envNameAllowed("windows", "ANTHROPIC_AUTH_TOKEN") {
		t.Error("windows 下也不该保留 ANTHROPIC_*")
	}
}

// claude 必须在配置目录下的专用空目录里跑；HOME 里的“项目设置”就是
// 用户的全局设置，正是 --setting-sources project 要避开的。
func TestPrepareWorkDirIsPrivate(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("STICKBOX_CONFIG_DIR", cfgDir)
	work, err := prepareWorkDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(work) != cfgDir || filepath.Base(work) != "work" {
		t.Errorf("工作目录 = %s，期望 %s/work", work, cfgDir)
	}
	di, err := os.Stat(work)
	if err != nil {
		t.Fatal(err)
	}
	if !di.IsDir() {
		t.Fatal("工作目录不是目录")
	}
	if runtime.GOOS != "windows" && di.Mode().Perm() != 0o700 {
		t.Errorf("工作目录权限 = %o，期望 700", di.Mode().Perm())
	}
}

func TestNormalizeServer(t *testing.T) {
	cases := []struct {
		in  string
		out string
		bad bool
	}{
		{"", defaultServer, false},
		{"https://mqtt.stickbox.ai/", "https://mqtt.stickbox.ai", false},
		{"https://stickbox.example:9443", "https://stickbox.example:9443", false},
		{"http://localhost:8080", "http://localhost:8080", false},
		{"http://127.0.0.1:19443/", "http://127.0.0.1:19443", false},
		{"http://[::1]:8080", "http://[::1]:8080", false},
		{"http://stickbox.example", "", true},
		{"http://192.168.1.5:8080", "", true},
		{"ftp://stickbox.example", "", true},
		{"stickbox.example", "", true},
	}
	for _, c := range cases {
		got, err := normalizeServer(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q 应该被拒绝，得到 %q", c.in, got)
			}
			continue
		}
		if err != nil || got != c.out {
			t.Errorf("%q = %q, %v；期望 %q", c.in, got, err, c.out)
		}
	}
}

func TestConfigSaveLoadAndPerms(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STICKBOX_CONFIG_DIR", dir)

	want := &agentConfig{
		Server:     "https://stickbox.example",
		Token:      "abc.def",
		ClaudePath: "/usr/local/bin/claude",
		Name:       "mac",
		PairedAt:   1792177433,
	}
	if err := saveConfig(want); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Errorf("读回 = %+v，期望 %+v", got, want)
	}
	if runtime.GOOS != "windows" {
		path, _ := configPath()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("配置文件权限 = %o，期望 600", fi.Mode().Perm())
		}
		if di, _ := os.Stat(filepath.Dir(path)); di.Mode().Perm() != 0o700 {
			t.Errorf("配置目录权限 = %o，期望 700", di.Mode().Perm())
		}
	}
}

func TestLoadConfigMissingIsEmpty(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "" || c.Server != "" {
		t.Errorf("缺失文件应返回空配置：%+v", c)
	}
}

func TestPostUsage(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("请求体不是 JSON：%v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":1,"devices":2}`))
	}))
	defer srv.Close()

	cfg := &agentConfig{Server: srv.URL, Token: "tok"}
	n, err := postUsage(cfg, usagePayload{
		FiveHour: &usageWindow{UsedPercentage: 2, ResetsAt: 1792192800},
		Limits:   json.RawMessage(`[{"kind":"session"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("devices = %d，期望 2", n)
	}
	if gotPath != "/api/usage" {
		t.Errorf("路径 = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody["five_hour"] == nil || gotBody["limits"] == nil {
		t.Errorf("请求体 = %v", gotBody)
	}
	if _, ok := gotBody["seven_day"]; ok {
		t.Errorf("seven_day 为空时不该出现：%v", gotBody)
	}
}

func TestTZCommands(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/tz" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotMethod = r.Method
		if r.Method == http.MethodPut {
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			gotBody = in["tz"]
			if gotBody == "" {
				fmt.Fprint(w, `{"tz":"Asia/Tokyo","source":"agent"}`)
			} else {
				fmt.Fprint(w, `{"tz":"Etc/GMT-7","source":"user"}`)
			}
			return
		}
		fmt.Fprint(w, `{"tz":"Asia/Jakarta","source":"user"}`)
	}))
	defer srv.Close()

	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	if err := saveConfig(&agentConfig{Server: srv.URL, Token: "k.s"}); err != nil {
		t.Fatal(err)
	}

	if err := cmdTZ([]string{"set", "+07:00"}); err != nil {
		t.Fatalf("tz set: %v", err)
	}
	if gotMethod != http.MethodPut || gotBody != "+07:00" {
		t.Fatalf("set 请求 = %s %q", gotMethod, gotBody)
	}

	if err := cmdTZ([]string{"show"}); err != nil {
		t.Fatalf("tz show: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("show 请求 = %s", gotMethod)
	}

	if err := cmdTZ([]string{"clear"}); err != nil {
		t.Fatalf("tz clear: %v", err)
	}
	if gotMethod != http.MethodPut || gotBody != "" {
		t.Fatalf("clear 请求 = %s %q", gotMethod, gotBody)
	}

	if err := cmdTZ([]string{"bogus"}); err == nil {
		t.Fatal("未知子命令没有报错")
	}
}

// pair：没令牌时不带 Authorization；有令牌时带上，把新屏并入同一用户。
func TestPairDeviceSendsExistingToken(t *testing.T) {
	var gotAuth string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"dev":"f412fa87c9e0","user_id":"u1","token":"newtok"}`))
	}))
	defer srv.Close()

	dev, token, err := pairDevice(&agentConfig{Server: srv.URL}, "123456", "mac")
	if err != nil {
		t.Fatal(err)
	}
	if dev != "f412fa87c9e0" || token != "newtok" {
		t.Errorf("dev=%q token=%q", dev, token)
	}
	if gotAuth != "" {
		t.Errorf("首次配对不该带令牌，拿到 %q", gotAuth)
	}
	if gotBody["code"] != "123456" || gotBody["name"] != "mac" {
		t.Errorf("请求体 = %v", gotBody)
	}

	_, _, err = pairDevice(&agentConfig{Server: srv.URL, Token: "old"}, "654321", "mac")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer old" {
		t.Errorf("已有令牌时应带上，拿到 %q", gotAuth)
	}
}

func TestLaunchdPlistUsesAbsolutePaths(t *testing.T) {
	p := launchdPlist("/opt/stickbox/agent", "/Users/x/Library/Logs/stickbox-agent.log", "")
	for _, want := range []string{
		"<string>ai.stickbox.agent</string>",
		"<string>/opt/stickbox/agent</string>",
		"<string>run</string>",
		"<string>/Users/x/Library/Logs/stickbox-agent.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist 缺少 %q：\n%s", want, p)
		}
	}
}

func TestSystemdUnitUsesAbsolutePath(t *testing.T) {
	u := systemdUnit("/opt/stickbox/agent", "")
	if !strings.Contains(u, "ExecStart=/opt/stickbox/agent run") {
		t.Errorf("unit 缺少绝对路径 ExecStart：\n%s", u)
	}
}

// 未配对时 install 必须直接拒绝，否则 launchd 会装一个永远失败的服务。
func TestInstallRequiresPairing(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	err := cmdInstall(nil)
	if !errors.Is(err, errNotPaired) {
		t.Fatalf("未配对时 install = %v，期望 errNotPaired", err)
	}
}

// KeepAlive 只在异常退出时拉起；Run/ThrottleInterval 不再造成 10 秒死循环。
func TestLaunchdPlistKeepAliveOnlyOnFailure(t *testing.T) {
	p := launchdPlist("/opt/stickbox/agent", "/tmp/agent.log", "")
	if !strings.Contains(p, "<key>KeepAlive</key>") ||
		!strings.Contains(p, "<key>SuccessfulExit</key><false/>") {
		t.Errorf("plist 的 KeepAlive 应为 SuccessfulExit=false：\n%s", p)
	}
	if strings.Contains(p, "<key>KeepAlive</key><true/>") {
		t.Errorf("plist 仍在无条件 KeepAlive：\n%s", p)
	}
}

// Background 进程会被 macOS 强力节流，claude 的网络往返会被拖到超时。
func TestLaunchdPlistDoesNotThrottle(t *testing.T) {
	p := launchdPlist("/opt/stickbox/agent", "/tmp/agent.log", "")
	if strings.Contains(p, "<key>ProcessType</key>") || strings.Contains(p, "Background") {
		t.Errorf("plist 不应再设置 ProcessType=Background：\n%s", p)
	}
	if !strings.Contains(p, "<key>LowPriorityIO</key><false/>") {
		t.Errorf("plist 应显式 LowPriorityIO=false：\n%s", p)
	}
}

func TestTailText(t *testing.T) {
	if got := tailText(nil, 600); got != "" {
		t.Errorf("空输入 = %q", got)
	}
	short := tailText([]byte("  hello\n"), 600)
	if short != "hello" {
		t.Errorf("短文本 = %q", short)
	}
	long := strings.Repeat("x", 1000) + "THE-END"
	got := tailText([]byte(long), 600)
	if len(got) != 600 || !strings.HasSuffix(got, "THE-END") {
		t.Errorf("tailText 长度 = %d，末尾 = %q", len(got), got[max(0, len(got)-20):])
	}
}

// 超时错误必须带上 claude 的 stderr 线索。
func TestClaudeExecErrorIncludesStderr(t *testing.T) {
	err := claudeExecError(errors.New("killed"), true, "network timeout")
	msg := err.Error()
	if !strings.Contains(msg, "超时") || !strings.Contains(msg, "network timeout") {
		t.Errorf("超时错误 = %q", msg)
	}
	err = claudeExecError(errors.New("exit 2"), false, "auth failed")
	if msg := err.Error(); !strings.Contains(msg, "auth failed") || !strings.Contains(msg, "exit 2") {
		t.Errorf("普通错误 = %q", msg)
	}
}

// 临时配置目录必须带进 launchd，否则服务读的是另一份配置。
func TestLaunchdPlistPropagatesConfigDir(t *testing.T) {
	p := launchdPlist("/opt/stickbox/agent", "/tmp/agent.log", "/tmp/cfg dir")
	for _, want := range []string{
		"<key>EnvironmentVariables</key>",
		"<key>STICKBOX_CONFIG_DIR</key>",
		"<string>/tmp/cfg dir</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist 缺少 %q：\n%s", want, p)
		}
	}
	if bare := launchdPlist("/opt/stickbox/agent", "/tmp/agent.log", ""); strings.Contains(bare, "STICKBOX_CONFIG_DIR") {
		t.Errorf("未设置 STICKBOX_CONFIG_DIR 时不该写进 plist：\n%s", bare)
	}
}

// 生成的 plist 必须能被 launchd 解析。
func TestLaunchdPlistIsValidPlist(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plutil 只在 macOS 上可用")
	}
	path := filepath.Join(t.TempDir(), "agent.plist")
	plist := launchdPlist("/opt/stickbox/agent", "/tmp/agent.log", "/tmp/cfg dir")
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint 失败：%v %s", err, out)
	}
}

func TestSystemdUnitPropagatesConfigDir(t *testing.T) {
	u := systemdUnit("/opt/stickbox/agent", "/tmp/cfg dir")
	for _, want := range []string{
		`Environment="STICKBOX_CONFIG_DIR=/tmp/cfg dir"`,
		"Restart=on-failure",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit 缺少 %q：\n%s", want, u)
		}
	}
	if bare := systemdUnit("/opt/stickbox/agent", ""); strings.Contains(bare, "STICKBOX_CONFIG_DIR") {
		t.Errorf("未设置 STICKBOX_CONFIG_DIR 时不该写进 unit：\n%s", bare)
	}
}

// Windows 用批处理包装器传环境变量（schtasks 本身不支持）。
func TestWindowsWrapperPropagatesConfigDir(t *testing.T) {
	w := windowsWrapper(`C:\Users\x\stickbox`, `C:\Program Files\stickbox-agent.exe`)
	for _, want := range []string{
		`set "STICKBOX_CONFIG_DIR=C:\Users\x\stickbox"`,
		`"C:\Program Files\stickbox-agent.exe" run`,
	} {
		if !strings.Contains(w, want) {
			t.Errorf("包装器缺少 %q：\n%s", want, w)
		}
	}
	if bare := windowsWrapper("", `C:\agent.exe`); strings.Contains(bare, "STICKBOX_CONFIG_DIR") {
		t.Errorf("未设置 STICKBOX_CONFIG_DIR 时不该写进包装器：\n%s", bare)
	}
}

// run 遇到 401 / 未配对不能退出（launchd 会重启），而是 1 小时后再试。
func TestRunDaemonRetryDelays(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"unauthorized", fmt.Errorf("上报失败：%w", &apiError{Status: http.StatusUnauthorized}), authRetryInterval},
		{"not paired", errNotPaired, authRetryInterval},
		{"other", errors.New("boom"), usageInterval},
		{"success", nil, usageInterval},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got time.Duration
			sleep := func(_ context.Context, d time.Duration) bool {
				got = d
				return false // 第一轮之后退出循环
			}
			err := runDaemon(context.Background(),
				func(string, ...any) {},
				func() error { return c.err },
				sleep)
			if err != nil {
				t.Fatalf("runDaemon 返回错误：%v", err)
			}
			if got != c.want {
				t.Errorf("重试间隔 = %s，期望 %s", got, c.want)
			}
		})
	}
}
