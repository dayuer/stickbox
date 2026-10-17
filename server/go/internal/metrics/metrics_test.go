package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRenderText(t *testing.T) {
	m := New()
	m.SetGaugeProviders(func() int { return 3 }, func() int { return 2 })
	m.IncUsageReport()
	m.IncUsageReport()
	m.ObserveRender(150*time.Millisecond, true)
	m.ObserveRender(50*time.Millisecond, false)
	m.ObserveFrameBytes(200 << 10)
	m.IncPairOK()
	m.IncPairFail()
	m.IncRateLimited("reg_dev")

	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`stickbox_online_devices 3`,
		`stickbox_mqtt_connections 2`,
		`stickbox_usage_reports_total 2`,
		`stickbox_render_failures_total 1`,
		`stickbox_render_seconds_count 2`,
		`stickbox_render_seconds_bucket{le="0.05"} 1`,
		`stickbox_render_seconds_bucket{le="0.25"} 2`,
		`stickbox_frame_package_bytes_count 1`,
		`stickbox_pair_total{result="ok"} 1`,
		`stickbox_pair_total{result="fail"} 1`,
		`stickbox_rate_limited_total{limiter="reg_dev"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少指标行 %q\n%s", want, body)
		}
	}
}

func TestOnlyMetricsPath(t *testing.T) {
	rr := httptest.NewRecorder()
	New().Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != 404 {
		t.Errorf("非 /metrics = %d，期望 404", rr.Code)
	}
}

func TestServeRejectsNonLoopback(t *testing.T) {
	if _, err := New().Serve("0.0.0.0:0"); err == nil {
		t.Error("监听 0.0.0.0 应当被拒绝")
	}
	if _, err := New().Serve(":9090"); err == nil {
		t.Error("空 host 应当被拒绝")
	}
}
