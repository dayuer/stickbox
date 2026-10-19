package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want outcome
	}{
		{"ok", nil, outcomeOK},
		{"skip limits null", &skipError{reason: "rate_limits.limits 为 null"}, outcomeTransient},
		{"5xx", fmt.Errorf("上报失败：%w", &apiError{Status: 502}), outcomeTransient},
		{"500", fmt.Errorf("上报失败：%w", &apiError{Status: 500}), outcomeTransient},
		{"401", fmt.Errorf("上报失败：%w", &apiError{Status: 401, Code: "unauthorized"}), outcomePersistent},
		{"403", fmt.Errorf("上报失败：%w", &apiError{Status: 403}), outcomePersistent},
		{"400", fmt.Errorf("上报失败：%w", &apiError{Status: 400}), outcomePersistent},
		{"429", fmt.Errorf("上报失败：%w", &apiError{Status: 429}), outcomePersistent},
		{"transport", fmt.Errorf("上报失败：%w", &transportError{msg: "连接失败", err: errors.New("i/o timeout")}), outcomeTransient},
		{"not paired", errNotPaired, outcomePersistent},
		{"not logged in", errClaudeNotLoggedIn, outcomePersistent},
		{"no report", &noReportError{}, outcomePersistent},
		{"parse", fmt.Errorf("解析 rate_limits.limits 失败：%w", errors.New("bad")), outcomePersistent},
		{"unknown", errors.New("boom"), outcomePersistent},
	}
	for _, c := range cases {
		if got := classify(c.err); got != c.want {
			t.Errorf("%s: classify = %s，期望 %s", c.name, got, c.want)
		}
	}
}

// 真实的 HTTP 超时 / 连接被拒 / 500 经过 apiClient.call 后分别归类正确。
func TestClassifyRealHTTPFailures(t *testing.T) {
	// 5xx
	srv5 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"e":"boom"}`, http.StatusBadGateway)
	}))
	defer srv5.Close()
	if _, err := postUsage(&agentConfig{Server: srv5.URL, Token: "t"}, usagePayload{}); classify(err) != outcomeTransient {
		t.Errorf("502 应为 transient：%v", err)
	}

	// 401
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"e":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv4.Close()
	if _, err := postUsage(&agentConfig{Server: srv4.URL, Token: "t"}, usagePayload{}); classify(err) != outcomePersistent {
		t.Errorf("401 应为 persistent：%v", err)
	}

	// 连接被拒：关掉服务端后再请求
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	_, err := postUsage(&agentConfig{Server: url, Token: "t"}, usagePayload{})
	if classify(err) != outcomeTransient {
		t.Errorf("连接被拒应为 transient：%v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "连接") {
		t.Errorf("错误文本应保持原样：%v", err)
	}
}

// 5xx 以外的 TLS 证书错误不会自己好，不该快速重试。
func TestClassifyTLSCertIsPersistent(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := postUsage(&agentConfig{Server: srv.URL, Token: "t"}, usagePayload{})
	if err == nil || classify(err) != outcomePersistent {
		t.Errorf("自签证书应为 persistent：%v", err)
	}
}

func TestFastRetryScheduleIsSixtyOneTwentyTwoForty(t *testing.T) {
	want := []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second}
	if len(fastRetryDelays) != len(want) {
		t.Fatalf("fastRetryDelays = %v", fastRetryDelays)
	}
	for i := range want {
		if fastRetryDelays[i] != want[i] {
			t.Errorf("第 %d 次 = %s，期望 %s", i+1, fastRetryDelays[i], want[i])
		}
	}
}
