package httpapi

import (
	"strings"
	"testing"
)

type fakeHealth bool

func (f fakeHealth) Healthy() bool { return bool(f) }

func TestHealthzOKWithoutAuth(t *testing.T) {
	h, _ := newTestAPI(t)
	rr := get(h, "/healthz", nil)
	if rr.Code != 200 {
		t.Fatalf("healthz = %d %s", rr.Code, rr.Body)
	}
	if strings.TrimSpace(rr.Body.String()) != "ok" {
		t.Errorf("body = %q", rr.Body)
	}
}

func TestHealthzBrokerDown(t *testing.T) {
	h, _ := newTestAPIWithHealth(t, fakeHealth(false))
	rr := get(h, "/healthz", nil)
	if rr.Code != 503 {
		t.Fatalf("healthz = %d，期望 503", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "mqtt") || strings.Contains(rr.Body.String(), "sqlite") {
		t.Errorf("响应泄露了内部信息: %q", rr.Body)
	}
}

func TestHealthzDBClosed(t *testing.T) {
	h, dep := newTestAPI(t)
	dep.store.Close()
	if rr := get(h, "/healthz", nil); rr.Code != 503 {
		t.Errorf("数据库关闭后 healthz = %d，期望 503", rr.Code)
	}
}
