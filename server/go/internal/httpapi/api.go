// Package httpapi 是纯 JSON API，没有网页。
//
// T7 起没有手机号、短信和 App：设备注册后出配对码，agent 输码完成绑定。
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/config"
	"github.com/dayuer/stickbox/server/go/internal/frames"
	"github.com/dayuer/stickbox/server/go/internal/registry"
	"github.com/dayuer/stickbox/server/go/internal/store"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

// Publisher 是 httpapi 对 MQTT 的窄依赖：推用量、通知配对状态、踢连接。
type Publisher interface {
	PublishUsage(dev string, payload []byte) error
	PublishPair(dev string, payload []byte) error
	KickDevice(dev string)
	// CancelDevice 结案该设备未确认的指令（解绑后不再重投）。
	CancelDevice(dev, reason string) int
}

type API struct {
	cfg   *config.Config
	store *store.Store
	v     *auth.Verifier
	reg   *registry.Registry
	pub   Publisher
	usage *usage.Service
	// frames 在 main 里构造后注入；测试里可以为 nil。
	frames *frames.Service

	// health 由 main 接上（broker 是否在监听）；测试里可以为 nil。
	health HealthChecker

	// pairSent 记录本进程已经发布过的配对状态（dev -> "true:<key_id>" /
	// "false"）。broker 的 retained 也在本进程内存里，进程重启后两边同时
	// 清空；有了它，订阅时只在 retained 缺失（或状态变了）才补发，避免
	// 同一条 paired=true 发两遍、以及补发压过随后的解绑通知。
	pairMu   sync.Mutex
	pairSent map[string]string

	// 内存限流：注册、换配对码、配对失败。
	regDev        *rateLimiter
	regIP         *rateLimiter
	pairCodeDev   *rateLimiter
	pairFailIP    *rateLimiter
	pairFailWorld *rateLimiter
	// 崩溃包：每台设备每小时一次。正常设备开机传一次就擦了。
	coredumpDev *rateLimiter
}

func New(cfg *config.Config, st *store.Store, v *auth.Verifier,
	reg *registry.Registry, pub Publisher) *API {
	return &API{
		cfg: cfg, store: st, v: v, reg: reg, pub: pub,
		usage: usage.NewService(st, pub, time.Now),

		regDev:        newRateLimiter("reg_dev", 5, 10*time.Minute),
		regIP:         newRateLimiter("reg_ip", 20, 10*time.Minute),
		pairCodeDev:   newRateLimiter("pair_code_dev", 10, 10*time.Minute),
		pairFailIP:    newRateLimiter("pair_fail_ip", 10, 10*time.Minute),
		pairFailWorld: newRateLimiter("pair_fail_world", 100, time.Minute),
		coredumpDev:   newRateLimiter("coredump_dev", 1, time.Hour),
	}
}

// RegisterQuota 返回某台设备注册限流的剩余次数与最早恢复时间，
// 给服务器本机的 device show 诊断用。
func (a *API) RegisterQuota(dev string) (left, limit int, reset time.Time) {
	return a.regDev.remaining(dev)
}

// Usage 暴露用量服务，main 拿它接到 broker 上（设备订阅时补发）。
func (a *API) Usage() *usage.Service { return a.usage }

// SetFrames 注入帧服务。构造 API 时还没有渲染器，所以用 setter。
func (a *API) SetFrames(f *frames.Service) { a.frames = f }

// HealthChecker 是 /healthz 对 broker 的窄依赖。
type HealthChecker interface{ Healthy() bool }

func (a *API) SetHealth(h HealthChecker) { a.health = h }

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// 不鉴权、不暴露内部信息，只回答 200/503。
	mux.HandleFunc("GET /healthz", a.handleHealthz)

	// 设备注册不鉴权：设备还没有密钥，这正是它来领密钥的入口。
	mux.HandleFunc("POST /api/device/register", a.handleDeviceRegister)
	mux.Handle("POST /api/device/pair-code", a.requireDevice(a.handleDevicePairCode))
	mux.Handle("GET /api/device/frame", a.requireDevice(a.handleDeviceFrame))
	mux.Handle("GET /api/device/firmware", a.requireDevice(a.handleDeviceFirmware))
	// 固件二进制不鉴权：开源硬件，bin 本身不是秘密，防篡改靠签名不靠访问控制。
	mux.HandleFunc("GET /fw/{name}", a.handleFirmwareFile)
	mux.Handle("POST /api/device/coredump", a.requireDevice(a.handleDeviceCoredump))

	// agent：pair 可选令牌（新用户不带，已有用户带着把新屏并入）。
	mux.HandleFunc("POST /api/agent/pair", a.handleAgentPair)
	mux.Handle("POST /api/agent/unpair", a.requireAgent(a.handleAgentUnpair))
	mux.Handle("GET /api/agent/devices", a.requireAgent(a.handleAgentDevices))
	mux.Handle("GET /api/agent/usage", a.requireAgent(a.handleUsageGet))
	mux.Handle("POST /api/usage", a.requireAgent(a.handleUsagePost))
	mux.Handle("GET /api/agent/coredumps", a.requireAgent(a.handleAgentCoredumps))
	mux.Handle("POST /api/agent/logout", a.requireAgent(a.handleAgentLogout))

	return drainBody(mux)
}

// handleHealthz 检查两件事：sqlite 能读、MQTT broker 还在监听。
// 失败时不返回任何内部错误文本，避免把文件路径/连接地址暴露出去。
func (a *API) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.Health(ctx); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("unavailable\n"))
		return
	}
	if a.health != nil && !a.health.Healthy() {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("unavailable\n"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

// 单次最多排掉多少请求体。设备自己的请求都 <4KB，1MB 已经很宽松。
const maxDrainBytes = 1 << 20

// drainBody 保证带请求体的请求在响应之后把体读完。
//
// 这条是真踩过的故障：旧服务端对 POST 直接返回 404 而不读 body，
// HTTP/1.1 长连接下连接失步，设备卡住直到超时重启。
func drainBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.Body == nil {
			return
		}
		n, err := io.Copy(io.Discard, io.LimitReader(r.Body, maxDrainBytes))
		if err != nil || n == maxDrainBytes {
			w.Header().Set("Connection", "close")
		}
	})
}

type handlerFn func(http.ResponseWriter, *http.Request, auth.Identity)

func (a *API) requireDevice(fn handlerFn) http.Handler {
	return a.guard(fn, auth.RoleDevice)
}
func (a *API) requireAgent(fn handlerFn) http.Handler {
	return a.guard(fn, auth.RoleAgent)
}

func (a *API) guard(fn handlerFn, want auth.Role) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok {
			unauthorized(w)
			return
		}
		id, err := a.v.Verify(tok)
		if err != nil {
			// 刻意不区分「不存在」和「密钥错」——不给探测者提供信息。
			unauthorized(w)
			return
		}
		if id.Role != want {
			unauthorized(w)
			return
		}
		fn(w, r, id)
	})
}

// ownsDevice 判定 agent 是否拥有这台屏。
func (a *API) ownsDevice(userID, dev string) bool {
	if userID == "" || dev == "" {
		return false
	}
	owner, bound, err := a.store.OwnerOfDevice(dev)
	return err == nil && bound && owner == userID
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	return h[len(p):], true
}

func unauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]string{"e": "unauthorized"})
}

// writeJSON 先编码再发头，为的是带上 Content-Length。
//
// 直接 WriteHeader 再 Encode 的话 Go 会用 chunked 传输编码，而设备用的
// esp_http_client **不解 chunked 响应体**。
func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"e":"server error"}`, http.StatusInternalServerError)
		return
	}
	b = append(b, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(code)
	w.Write(b)
}

func fail(w http.ResponseWriter, code int, e, detail string) {
	m := map[string]string{"e": e}
	if detail != "" {
		m["detail"] = detail
	}
	writeJSON(w, code, m)
}

// decodeJSON 读一个很小的 JSON 请求体。设备/agent 的请求都远小于 4KB。
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(v); err != nil {
		fail(w, 400, "bad request", "")
		return false
	}
	return true
}

// clientIP 返回请求的真实来源地址。
//
// 生产环境 HTTPS 走本机 nginx 反代，RemoteAddr 全是 127.0.0.1；只有
// 来源是回环时才采用 X-Forwarded-For（nginx 用 $remote_addr 覆盖写，
// 只有一个值）。其余情况一律用 RemoteAddr，绝不信任客户端自带的头。
func clientIP(r *http.Request) string {
	if ip := addrIP(r.RemoteAddr); ip != nil && ip.IsLoopback() {
		if xff := firstForwardedFor(r.Header.Get("X-Forwarded-For")); xff != "" {
			return xff
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func addrIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return net.ParseIP(host)
}

func firstForwardedFor(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// rateKeyIP 把来源地址归一化后作为限流键：
// IPv4 用完整地址，IPv6 按 /64 聚合——否则换一个地址就能绕过每 IP 限制。
func rateKeyIP(r *http.Request) string {
	raw := clientIP(r)
	ip := net.ParseIP(raw)
	if ip == nil {
		return raw
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	if v6 := ip.To16(); v6 != nil {
		return v6.Mask(net.CIDRMask(64, 128)).String()
	}
	return raw
}
