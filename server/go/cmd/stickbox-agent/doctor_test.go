package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestDoctor(t *testing.T, cfg *agentConfig) (*doctor, *bytes.Buffer) {
	t.Helper()
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	oldDetector := thirdPartyDetector
	thirdPartyDetector = func() string { return "" }
	t.Cleanup(func() { thirdPartyDetector = oldDetector })
	d := newDoctor(cfg)
	var buf bytes.Buffer
	d.out = &buf
	d.now = func() time.Time {
		return time.Date(2026, 10, 17, 12, 0, 0, 0, time.Local)
	}
	d.logPath = filepath.Join(t.TempDir(), "stickbox-agent.log")
	return d, &buf
}

func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoctorClaudeVersion(t *testing.T) {
	cfg := &agentConfig{ClaudePath: fakeClaude(t, `echo "2.1.999 (Claude Code)"`)}
	d, _ := newTestDoctor(t, cfg)
	r := d.checkClaude()
	if r.status != statusOK || !strings.Contains(r.detail, "2.1.999") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorClaudeMissing(t *testing.T) {
	cfg := &agentConfig{ClaudePath: filepath.Join(t.TempDir(), "nope")}
	d, _ := newTestDoctor(t, cfg)
	r := d.checkClaude()
	if r.status != statusFail || !strings.Contains(r.fix, "安装") {
		t.Fatalf("结果 = %+v", r)
	}
}

// 真正跑一次假 claude：验证白名单环境 + 专用工作目录下能解析出用量。
func TestDoctorUsageReadsRateLimits(t *testing.T) {
	fixture, err := filepath.Abs("testdata/usage-stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &agentConfig{ClaudePath: fakeClaude(t, "cat "+fixture)}
	d, _ := newTestDoctor(t, cfg)
	r := d.checkUsage()
	if r.status != statusOK || !strings.Contains(r.detail, "五小时") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorUsageRateLimited(t *testing.T) {
	d, _ := newTestDoctor(t, &agentConfig{})
	d.runUsage = func() (usagePayload, string, error) {
		return usagePayload{}, "", errors.New("claude 执行失败：exit status 1；stderr 末尾：Error: 429 Too Many Requests")
	}
	r := d.checkUsage()
	if r.status != statusFail || !strings.Contains(r.detail, "冷却") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorUsageNotLoggedIn(t *testing.T) {
	d, _ := newTestDoctor(t, &agentConfig{})
	d.runUsage = func() (usagePayload, string, error) {
		return usagePayload{}, "", errors.New("claude 执行失败：exit status 1；stderr 末尾：Not logged in. Please run `claude login`")
	}
	r := d.checkUsage()
	if r.status != statusFail || !strings.Contains(r.fix, "登录") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorUsageThirdParty(t *testing.T) {
	d, _ := newTestDoctor(t, &agentConfig{})
	thirdPartyDetector = func() string { return "ANTHROPIC_BASE_URL" }
	d.runUsage = func() (usagePayload, string, error) {
		return usagePayload{}, "claude 没有返回限额行（rate_limits.limits 为 null），本轮跳过", nil
	}
	r := d.checkUsage()
	if r.status != statusWarn || !strings.Contains(r.fix, "第三方") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorTokenInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"e":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	cfg := &agentConfig{Server: srv.URL, Token: "k.s"}
	d, _ := newTestDoctor(t, cfg)
	r := d.checkToken()
	if r.status != statusFail || !strings.Contains(r.detail, "401") || !strings.Contains(r.fix, "pair") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorTokenValid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"devices":[{"dev":"f412fa87c9e0"}]}`)
	}))
	defer srv.Close()
	cfg := &agentConfig{Server: srv.URL, Token: "k.s"}
	d, _ := newTestDoctor(t, cfg)
	r := d.checkToken()
	if r.status != statusOK || !strings.Contains(r.detail, "1 台") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorTokenMissing(t *testing.T) {
	d, _ := newTestDoctor(t, &agentConfig{})
	r := d.checkToken()
	if r.status != statusFail || !strings.Contains(r.fix, "pair") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorServerHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			fmt.Fprint(w, "ok")
		}
	}))
	defer srv.Close()
	d, _ := newTestDoctor(t, &agentConfig{Server: srv.URL})
	r := d.checkServer()
	if r.status != statusOK {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorServerDNSFailure(t *testing.T) {
	d, _ := newTestDoctor(t, &agentConfig{Server: "https://mqtt.stickbox.ai"})
	d.lookupHost = func(string) ([]string, error) { return nil, errors.New("no such host") }
	r := d.checkServer()
	if r.status != statusFail || !strings.Contains(r.fix, "DNS") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorServerTLSCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	d, _ := newTestDoctor(t, &agentConfig{Server: srv.URL})
	r := d.checkServer()
	if r.status != statusFail || !strings.Contains(r.fix, "证书") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorServiceLastReport(t *testing.T) {
	d, _ := newTestDoctor(t, &agentConfig{})
	d.service = func() (bool, string) { return true, "launchd" }
	content := "2026/10/17 11:40:00 本轮失败：x\n" +
		"2026/10/17 11:50:00 已上报：五小时 37% / 七天 19%；服务端推送到 1 台设备\n"
	if err := os.WriteFile(d.logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	r := d.checkService()
	if r.status != statusOK || !strings.Contains(r.detail, "10 分钟") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorServiceNotInstalled(t *testing.T) {
	d, _ := newTestDoctor(t, &agentConfig{})
	d.service = func() (bool, string) { return false, "" }
	r := d.checkService()
	if r.status != statusWarn || !strings.Contains(r.fix, "install") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorServiceNoReport(t *testing.T) {
	d, _ := newTestDoctor(t, &agentConfig{})
	d.service = func() (bool, string) { return true, "systemd" }
	if err := os.WriteFile(d.logPath, []byte("2026/10/17 11:50:00 启动\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := d.checkService()
	if r.status != statusWarn || !strings.Contains(r.detail, "没有成功上报") {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorTimezone(t *testing.T) {
	d, _ := newTestDoctor(t, &agentConfig{})
	d.tz = func() string { return "Asia/Bangkok" }
	if r := d.checkTZ(); r.status != statusOK || r.detail != "Asia/Bangkok" {
		t.Fatalf("结果 = %+v", r)
	}
	d.tz = func() string { return "" }
	if r := d.checkTZ(); r.status != statusWarn {
		t.Fatalf("结果 = %+v", r)
	}
}

func TestDoctorRunAllGood(t *testing.T) {
	fixture, err := filepath.Abs("testdata/usage-stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`if [ "$1" = "--version" ]; then echo "2.1.999"; else cat %s; fi`, fixture)
	cfg := &agentConfig{ClaudePath: fakeClaude(t, script), Server: "https://example.com", Token: "k.s"}
	d, buf := newTestDoctor(t, cfg)
	d.runUsage = func() (usagePayload, string, error) {
		return usagePayload{FiveHour: &usageWindow{UsedPercentage: 37, ResetsAt: 1792244076}}, "", nil
	}
	d.apiCall = func(string, string, any) error { return nil }
	d.lookupHost = func(string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	d.checkHealth = func(string) error { return nil }
	d.service = func() (bool, string) { return true, "launchd" }
	d.tz = func() string { return "Asia/Bangkok" }
	if err := os.WriteFile(d.logPath, []byte("2026/10/17 11:59:00 已上报：x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.run(); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"claude 可执行文件", "用量读取", "配对令牌", "服务端连通性", "后台服务", "时区", "1 分钟"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q：\n%s", want, out)
		}
	}
}

func TestDoctorRunReportsFailure(t *testing.T) {
	d, buf := newTestDoctor(t, &agentConfig{})
	d.runUsage = func() (usagePayload, string, error) {
		return usagePayload{}, "", errors.New("not logged in")
	}
	d.apiCall = func(string, string, any) error { return nil }
	d.lookupHost = func(string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	d.checkHealth = func(string) error { return nil }
	d.service = func() (bool, string) { return false, "" }
	d.tz = func() string { return "Asia/Bangkok" }
	if err := d.run(); err == nil {
		t.Fatal("没有令牌时应返回失败")
	}
	if !strings.Contains(buf.String(), "FAIL") {
		t.Errorf("输出没有 FAIL：\n%s", buf.String())
	}
}
