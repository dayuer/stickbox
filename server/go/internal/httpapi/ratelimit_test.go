package httpapi

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterPrunesEmptyKeys(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRateLimiter("test", 2, time.Minute)
	l.now = func() time.Time { return now }

	l.hit("a")
	now = now.Add(2 * time.Minute)
	if l.exceeded("a") {
		t.Fatal("窗口已过，不该超限")
	}
	l.mu.Lock()
	_, ok := l.hits["a"]
	l.mu.Unlock()
	if ok {
		t.Error("过期后空键没删掉，map 会无限增长")
	}
}

func TestRateKeyIPNormalizesIPv6(t *testing.T) {
	key := func(addr string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = addr
		return rateKeyIP(r)
	}

	if a, b := key("[2001:db8:1:2::1]:1111"), key("[2001:db8:1:2::ffff]:2222"); a != b {
		t.Errorf("同一 /64 的两个地址给出了不同键：%q vs %q", a, b)
	}
	if a, b := key("[2001:db8:1:2::1]:1111"), key("[2001:db8:1:3::1]:2222"); a == b {
		t.Errorf("不同 /64 的地址被合并：%q", a)
	}
	if got := key("203.0.113.5:1234"); got != "203.0.113.5" {
		t.Errorf("IPv4 = %q", got)
	}
	if got := key("[::ffff:203.0.113.5]:1234"); got != "203.0.113.5" {
		t.Errorf("IPv4-mapped = %q", got)
	}
}

// 反代：只有回环来源才信 X-Forwarded-For；直连来源自带的头一律忽略。
func TestClientIPTrustsXFFOnlyFromLoopback(t *testing.T) {
	withXFF := func(remote, xff string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return clientIP(r)
	}

	if got := withXFF("127.0.0.1:1234", "203.0.113.9"); got != "203.0.113.9" {
		t.Errorf("回环 + XFF = %q，期望 203.0.113.9", got)
	}
	if got := withXFF("[::1]:1234", "203.0.113.9"); got != "203.0.113.9" {
		t.Errorf("IPv6 回环 + XFF = %q", got)
	}
	if got := withXFF("192.168.3.50:1234", "203.0.113.9"); got != "192.168.3.50" {
		t.Errorf("非回环来源的信头不该被信任，得到 %q", got)
	}
	if got := withXFF("127.0.0.1:1234", "203.0.113.9, 198.51.100.1"); got != "203.0.113.9" {
		t.Errorf("多值 XFF 取第一个，得到 %q", got)
	}

	// XFF 里的 IPv6 仍按 /64 作为限流键。
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "2001:db8:9:8::1")
	if got := rateKeyIP(r); got != "2001:db8:9:8::" {
		t.Errorf("XFF IPv6 限流键 = %q", got)
	}
}
