package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSecretPath = "/r/SECRETSECRETSECRETSECRETSECRETSECRETSECRET1"

func TestParseLinkOrigin(t *testing.T) {
	ok := map[string]string{
		"https://mqtt.stickbox.ai" + testSecretPath: "https://mqtt.stickbox.ai",
		"http://127.0.0.1:8080" + testSecretPath:    "http://127.0.0.1:8080",
		"http://localhost:9" + testSecretPath:       "http://localhost:9",
	}
	for in, want := range ok {
		if got, err := parseLinkOrigin(in); err != nil || got != want {
			t.Errorf("%q → %q %v", in, got, err)
		}
	}
	for _, in := range []string{
		"", "nonsense", "ftp://x.com" + testSecretPath,
		"http://example.com" + testSecretPath, // 明文外网
		"https://x.com/other/abc", "https://x.com/r/", "https://user:pw@x.com" + testSecretPath,
	} {
		if _, err := parseLinkOrigin(in); err == nil {
			t.Errorf("%q 应被拒绝", in)
		} else if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), in) && in != "" {
			t.Errorf("错误文本回显了链接：%v", err)
		}
	}
}

func fakeRedeemServer(t *testing.T, status int, body string, got *map[string]string, auth *string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/redeem" || r.Method != "POST" {
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		}
		if auth != nil {
			*auth = r.Header.Get("Authorization")
		}
		if got != nil {
			json.NewDecoder(r.Body).Decode(got)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPairWithLinkSavesToken(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	var got map[string]string
	var auth string
	srv := fakeRedeemServer(t, 200, `{"dev":"f412fa87c9e0","user_id":"u1","token":"tok123"}`, &got, &auth)
	link := srv.URL + testSecretPath

	var out bytes.Buffer
	if err := pairWithLink(link, "", "my-mac", false, &out); err != nil {
		t.Fatal(err)
	}
	if got["link"] != link || got["name"] != "my-mac" || auth != "" {
		t.Errorf("请求 %v auth=%q", got, auth)
	}
	c, err := loadConfig()
	if err != nil || c.Token != "tok123" || c.Server != srv.URL || c.PairedAt == 0 {
		t.Fatalf("配置 %+v %v", c, err)
	}
	if !strings.Contains(out.String(), "配对成功：f412fa87c9e0（新用户）") {
		t.Errorf("输出 = %q", out.String())
	}
	if strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), "tok123") {
		t.Errorf("输出泄露链接或令牌：%q", out.String())
	}
}

func TestPairWithLinkErrorsDoNotEchoLink(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{404, `{"e":"bad link"}`, "配对链接无效"},
		{429, `{"e":"rate limited"}`, "尝试次数过多"},
		{409, `{"e":"device bound","detail":"x"}`, "已绑定"},
	} {
		t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
		srv := fakeRedeemServer(t, tc.status, tc.body, nil, nil)
		err := pairWithLink(srv.URL+testSecretPath, "", "mac", false, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("status %d err=%v", tc.status, err)
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("错误回显了链接：%v", err)
		}
		if c, _ := loadConfig(); c.Token != "" {
			t.Errorf("失败不该保存令牌")
		}
	}
}

func TestPairWithLinkRefusesWhenAlreadyPaired(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	if err := saveConfig(&agentConfig{Server: "https://mqtt.stickbox.ai", Token: "old"}); err != nil {
		t.Fatal(err)
	}
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	if err := pairWithLink(srv.URL+testSecretPath, "", "mac", false, &bytes.Buffer{}); err == nil {
		t.Fatal("已有令牌应拒绝")
	}
	if called {
		t.Error("不该消耗一次性链接")
	}
	if c, _ := loadConfig(); c.Token != "old" {
		t.Errorf("令牌被改：%q", c.Token)
	}
}

func TestPairWithLinkServerMismatchAndScheme(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	if err := pairWithLink("http://example.com"+testSecretPath, "", "m", false, &bytes.Buffer{}); err == nil {
		t.Error("明文外网应拒绝")
	}
	if err := pairWithLink("https://a.example"+testSecretPath, "https://b.example", "m", false, &bytes.Buffer{}); err == nil ||
		strings.Contains(err.Error(), "SECRET") {
		t.Errorf("server 不一致应拒绝且不回显：%v", err)
	}
}

func TestCmdPairLinkConflicts(t *testing.T) {
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	if err := cmdPair([]string{"--link", "https://x.example" + testSecretPath, "123456"}); err == nil {
		t.Error("--link 与配对码同用应报错")
	}
	if err := cmdPair([]string{"--link", "https://x.example" + testSecretPath, "--auto"}); err == nil {
		t.Error("--link 与 --auto 同用应报错")
	}
}
