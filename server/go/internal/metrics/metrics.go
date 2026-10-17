// Package metrics 是一个很小的 Prometheus 文本格式导出器。
//
// 只依赖标准库：指标不多，没必要为几行文本引入 client_golang。
// 监听地址必须是回环地址（默认 127.0.0.1:9090），不经过 nginx、不对外。
package metrics

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var renderBuckets = []float64{.05, .1, .25, .5, 1, 2.5, 5, 10}
var frameBuckets = []float64{
	64 << 10, 128 << 10, 192 << 10, 256 << 10, 384 << 10, 512 << 10, 1 << 20,
}

type histogram struct {
	buckets []float64
	counts  []uint64
	count   uint64
	sum     float64
}

func newHistogram(buckets []float64) histogram {
	return histogram{buckets: buckets, counts: make([]uint64, len(buckets))}
}

func (h *histogram) observe(v float64) {
	h.count++
	h.sum += v
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
			return
		}
	}
}

// Registry 保存全部指标。Default 是进程级实例，业务代码直接用包级函数。
type Registry struct {
	mu sync.Mutex

	usageReports uint64
	render       histogram
	renderFails  uint64
	frames       histogram
	pairOK       uint64
	pairFail     uint64
	rateLimited  map[string]uint64

	onlineDevices func() int
	mqttClients   func() int
}

var Default = New()

func New() *Registry {
	return &Registry{
		render:      newHistogram(renderBuckets),
		frames:      newHistogram(frameBuckets),
		rateLimited: map[string]uint64{},
	}
}

// SetGaugeProviders 接上两个即时查询的 gauge。
func (m *Registry) SetGaugeProviders(online, mqtt func() int) {
	m.mu.Lock()
	m.onlineDevices, m.mqttClients = online, mqtt
	m.mu.Unlock()
}

func (m *Registry) IncUsageReport() {
	m.mu.Lock()
	m.usageReports++
	m.mu.Unlock()
}

func (m *Registry) ObserveRender(d time.Duration, ok bool) {
	m.mu.Lock()
	m.render.observe(d.Seconds())
	if !ok {
		m.renderFails++
	}
	m.mu.Unlock()
}

func (m *Registry) ObserveFrameBytes(n int) {
	m.mu.Lock()
	m.frames.observe(float64(n))
	m.mu.Unlock()
}

func (m *Registry) IncPairOK()   { m.mu.Lock(); m.pairOK++; m.mu.Unlock() }
func (m *Registry) IncPairFail() { m.mu.Lock(); m.pairFail++; m.mu.Unlock() }

func (m *Registry) IncRateLimited(limiter string) {
	m.mu.Lock()
	m.rateLimited[limiter]++
	m.mu.Unlock()
}

// 包级快捷方式，业务代码不用到处传 Registry。
func IncUsageReport()                        { Default.IncUsageReport() }
func ObserveRender(d time.Duration, ok bool) { Default.ObserveRender(d, ok) }
func ObserveFrameBytes(n int)                { Default.ObserveFrameBytes(n) }
func IncPairOK()                             { Default.IncPairOK() }
func IncPairFail()                           { Default.IncPairFail() }
func IncRateLimited(limiter string)          { Default.IncRateLimited(limiter) }

// Handler 输出 Prometheus 文本格式。
func (m *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write(m.renderText())
	})
}

func (m *Registry) renderText() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder
	online, mqtt := 0, 0
	if m.onlineDevices != nil {
		online = m.onlineDevices()
	}
	if m.mqttClients != nil {
		mqtt = m.mqttClients()
	}

	writeGauge(&b, "stickbox_online_devices", "最近 120 秒内发过心跳的设备数", online)
	writeGauge(&b, "stickbox_mqtt_connections", "当前 MQTT 连接数（不含服务端内联客户端）", mqtt)

	writeCounter(&b, "stickbox_usage_reports_total", "成功落库的用量上报次数", m.usageReports)
	writeCounter(&b, "stickbox_render_failures_total", "看板渲染失败次数", m.renderFails)
	writeHistogram(&b, "stickbox_render_seconds", "一次看板渲染的耗时", m.render)
	writeHistogram(&b, "stickbox_frame_package_bytes", "一次帧包（底帧+图集+索引）的字节数", m.frames)

	b.WriteString("# HELP stickbox_pair_total 配对结果计数\n")
	b.WriteString("# TYPE stickbox_pair_total counter\n")
	fmt.Fprintf(&b, "stickbox_pair_total{result=\"ok\"} %d\n", m.pairOK)
	fmt.Fprintf(&b, "stickbox_pair_total{result=\"fail\"} %d\n", m.pairFail)

	b.WriteString("# HELP stickbox_rate_limited_total 限流命中次数\n")
	b.WriteString("# TYPE stickbox_rate_limited_total counter\n")
	keys := make([]string, 0, len(m.rateLimited))
	for k := range m.rateLimited {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "stickbox_rate_limited_total{limiter=%q} %d\n", k, m.rateLimited[k])
	}
	return []byte(b.String())
}

func writeGauge(b *strings.Builder, name, help string, v int) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
}

func writeCounter(b *strings.Builder, name, help string, v uint64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
}

func writeHistogram(b *strings.Builder, name, help string, h histogram) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	var cum uint64
	for i, bound := range h.buckets {
		cum += h.counts[i]
		fmt.Fprintf(b, "%s_bucket{le=%q} %d\n", name, formatFloat(bound), cum)
	}
	fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n", name, h.count)
	fmt.Fprintf(b, "%s_sum %s\n", name, formatFloat(h.sum))
	fmt.Fprintf(b, "%s_count %d\n", name, h.count)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// Serve 在 addr 上起一个只在回环地址监听的 HTTP 服务。
// 返回的 Server 可在测试里 Close。
func (m *Registry) Serve(addr string) (*http.Server, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("metrics: 地址 %q 不合法: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("metrics: 只能监听回环地址，收到 %q", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		Handler:           m.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(ln)
	return srv, nil
}
