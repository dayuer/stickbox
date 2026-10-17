package httpapi

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterPrunesEmptyKeys(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRateLimiter(2, time.Minute)
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
