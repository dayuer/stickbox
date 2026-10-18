// Package broker 把 MQTT broker 塞进本进程。
//
// 内嵌之后推一条用量是一次内存中的函数调用，不再经网络。
// 认证和 ACL 也跟 HTTP 共用同一套密钥，不用维护第二份配置。
package broker

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

// CommandTracker 是 broker 对下行确认跟踪的窄依赖（见 internal/command）。
type CommandTracker interface {
	Send(dev, topic string, payload []byte, retain bool) (string, error)
	Ack(dev, cid string, ok bool, errMsg string)
	// CancelDevice 结案该设备所有未确认的指令（换密钥/吊销/解绑后不再重投）。
	CancelDevice(dev, reason string) int
}

// idTable 记住每条连接校验通过的身份，ACL 判定直接取，不重复跑 argon2。
//
// 键必须是**连接**而不是 client id。设备重连时 client id 不变，mochi 会做
// 会话接管：旧连接的 OnDisconnect 在新连接建立之后才触发，用 client id 做键
// 的话，那次清理会把新连接的身份一起删掉——此后这条连接的每次 ACL 检查都
// 查不到身份、一律拒绝，设备连自己的状态都发不出去，断开重连成死循环。
//
// 这个 bug 在实机上现形过：服务端日志里是
// `error="not authorized" TopicName:printer/{dev}/status`。
type idTable struct {
	mu sync.RWMutex
	m  map[*mqtt.Client]auth.Identity
}

func newIDTable() *idTable { return &idTable{m: map[*mqtt.Client]auth.Identity{}} }

// admit 在持有锁时完成「检查 + 登记」，避免 pending 和 active 同时
// 连接时都通过检查、随后互相顶号。
//
// pending 身份不能用**同一个 client ID** 顶掉已在线 active 连接；
// active 可以接管 pending（合法设备重新上线时这是正常的）。
// 不同 client ID 的 pending 连接不会触发 MQTT 会话接管，只受 ACL 限制
// （它读不到 usage/frame/ack），所以不拦——T18 的伪造 ACK 测试依赖这一点。
func (t *idTable) admit(cl *mqtt.Client, id auth.Identity) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id.Role == auth.RoleDevice && id.Pending {
		for cur, curID := range t.m {
			if curID.Role == auth.RoleDevice && curID.Dev == id.Dev &&
				!curID.Pending && cur.ID == cl.ID {
				return false
			}
		}
	}
	t.m[cl] = id
	return true
}

// dropWhere 移除满足 match 的身份，返回对应的连接。
//
// 用于「密钥已经被 CLI 吊销，但连接还挂着」：CLI 直接改库、不和 broker
// 同进程，没法主动踢线；下一次认证或心跳时由这里把失效身份清掉。
func (t *idTable) dropWhere(match func(auth.Identity) bool) []*mqtt.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*mqtt.Client
	for cl, id := range t.m {
		if !match(id) {
			continue
		}
		delete(t.m, cl)
		out = append(out, cl)
	}
	return out
}

// put 只给测试夹具直接登记身份用；生产路径走 admit。
func (t *idTable) put(cl *mqtt.Client, id auth.Identity) {
	t.mu.Lock()
	t.m[cl] = id
	t.mu.Unlock()
}

func (t *idTable) get(cl *mqtt.Client) (auth.Identity, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	id, ok := t.m[cl]
	return id, ok
}

func (t *idTable) drop(cl *mqtt.Client) {
	t.mu.Lock()
	delete(t.m, cl)
	t.mu.Unlock()
}

// packetRedactHandler 把 mochi 的 "error processing packet" 日志里的 pk
// 字段替换成占位符。ACL 拒绝时 mochi 会把整个包（含 payload）打进 WARN，
// 每包几 KB：既泄露内容，也能被刷日志。错误类型、client、topic 仍保留。
type packetRedactHandler struct{ slog.Handler }

func (h packetRedactHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message != "error processing packet" {
		return h.Handler.Handle(ctx, r)
	}
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "pk" {
			nr.AddAttrs(slog.String("pk", "<redacted>"))
		} else {
			nr.AddAttrs(a)
		}
		return true
	})
	return h.Handler.Handle(ctx, nr)
}

func (h packetRedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return packetRedactHandler{h.Handler.WithAttrs(attrs)}
}

func (h packetRedactHandler) WithGroup(name string) slog.Handler {
	return packetRedactHandler{h.Handler.WithGroup(name)}
}

type Broker struct {
	srv     *mqtt.Server
	hook    *Hook
	healthy atomic.Bool
	cmd     CommandTracker
}

// Options 是 broker 的容量参数。零值用下面的默认值。
type Options struct {
	// MaximumClientWritesPending 是每连接待写队列长度，直接决定每连接内存。
	MaximumClientWritesPending int32
	// MaximumInflight / ReceiveMaximum 是 QoS>0 在途消息上限。
	MaximumInflight uint16
	ReceiveMaximum  uint16
	// AdmitPerSec 是全局新连接准入（每秒），0 表示默认 50。
	AdmitPerSec int
}

func New(addr string, tlsCfg *tls.Config, v *auth.Verifier, r Router, opt Options) (*Broker, error) {
	if opt.MaximumClientWritesPending <= 0 {
		opt.MaximumClientWritesPending = 64
	}
	if opt.MaximumInflight == 0 {
		opt.MaximumInflight = 32
	}
	if opt.ReceiveMaximum == 0 {
		opt.ReceiveMaximum = 32
	}
	if opt.AdmitPerSec <= 0 {
		opt.AdmitPerSec = 50
	}
	// mochi 默认 8192/8192/1024，每连接实测约 1.4MB；设备只有
	// 3 个订阅、QoS1 流量很低，压到 64/32/32 足够。
	caps := mqtt.NewDefaultServerCapabilities()
	caps.MaximumClientWritesPending = opt.MaximumClientWritesPending
	caps.MaximumInflight = opt.MaximumInflight
	caps.ReceiveMaximum = opt.ReceiveMaximum
	srv := mqtt.New(&mqtt.Options{
		// 服务端要能直接 Publish 推用量，这个开关是前提；
		// 不开的话 Publish 返回「please set Options.InlineClient=true」。
		InlineClient: true,
		Capabilities: caps,
		// broker 自己的日志走 slog，别另起一套格式
		Logger: slog.New(packetRedactHandler{slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelWarn,
		})}),
	})
	h := &Hook{
		srv: srv, v: v, router: r, ids: newIDTable(),
		pending: newPendingLimiter(6, time.Minute),
		admit:   newAdmission(opt.AdmitPerSec),
		auth:    newAuthLog(200),
	}
	if err := srv.AddHook(h, nil); err != nil {
		return nil, err
	}
	l := listeners.NewTCP(listeners.Config{
		ID: "mqtt-tls", Address: addr, TLSConfig: tlsCfg,
	})
	if err := srv.AddListener(l); err != nil {
		return nil, err
	}
	return &Broker{srv: srv, hook: h}, nil
}

func (b *Broker) Serve() error {
	err := b.srv.Serve()
	if err == nil {
		b.healthy.Store(true)
	}
	return err
}

func (b *Broker) Close() error {
	b.healthy.Store(false)
	return b.srv.Close()
}

// Healthy 供 /healthz 用：监听已经起来且还没关闭。
func (b *Broker) Healthy() bool { return b.healthy.Load() }

// ClientCount 是当前 MQTT 连接数（排除服务端内联客户端）。
func (b *Broker) ClientCount() int {
	n := 0
	for _, cl := range b.srv.Clients.GetAll() {
		if !cl.Net.Inline {
			n++
		}
	}
	return n
}

// PublishRaw 是不带 cid、不跟踪确认的底层发布。command.Tracker 用它，
// 其余地方一律走下面两个带确认的入口。
func (b *Broker) PublishRaw(topic string, payload []byte, retain bool) error {
	return b.srv.Publish(topic, payload, retain, 1)
}

// PublishUsage 推 Claude 用量。retain=0：payload 里的 now 是现场盖的，
// 留着会过期；设备重连时由 OnSubscribed 触发补发一份新的（见 usage 包）。
//
// 经 command.Tracker 下发：带 cid，设备应用完回 ACK，没收到就重投。
// QoS1 只保证投到设备的 MQTT 栈，不保证它真的应用了（T18）。
func (b *Broker) PublishUsage(dev string, payload []byte) error {
	return b.send(dev, fmt.Sprintf("printer/%s/usage", dev), payload, false)
}

// PublishPair 通知设备配对状态。retain=1：设备离线时错过配对，
// 重连后仍能拿到「已配对 / 已解绑」的当前状态。
//
// pair 不做 ACK 跟踪，直接裸发布：它是可重放的状态通知，不携带会过期的
// 数据。retained 消息 + 设备订阅时的 ResendPair 已经保证最终一致；给
// pair 带 cid 反而会把一条发给旧密钥的解绑指令重投给重新注册的新密钥。
// usage/frame 仍然走 tracker（数据有时效，必须确认）。
func (b *Broker) PublishPair(dev string, payload []byte) error {
	return b.PublishRaw(fmt.Sprintf("printer/%s/pair", dev), payload, true)
}

// send 有 tracker 就走带确认的路，没有就退回裸发布（测试里、或还没接上时）。
func (b *Broker) send(dev, topic string, payload []byte, retain bool) error {
	if b.cmd == nil {
		return b.PublishRaw(topic, payload, retain)
	}
	_, err := b.cmd.Send(dev, topic, payload, retain)
	return err
}

// SetCommandTracker 接上下行指令的确认跟踪。用 setter 的原因同
// SetUsageResender：tracker 要用 broker 发消息，构造时还没有 broker。
func (b *Broker) SetCommandTracker(c CommandTracker) {
	b.cmd = c
	b.hook.cmd = c
}

// RecentAuth 返回某设备最近的 CONNECT 结果（新的在前），设备诊断用。
func (b *Broker) RecentAuth(dev string, n int) []AuthEvent {
	if b.hook == nil || b.hook.auth == nil {
		return nil
	}
	return b.hook.auth.recent(dev, n)
}

// ConnectedKeys 返回该设备当前在线连接使用的 key_id。
func (b *Broker) ConnectedKeys(dev string) []string {
	if b.hook == nil {
		return nil
	}
	return b.hook.connectedKeys(dev)
}

// CancelDevice 结案该设备所有未确认的指令。换密钥/吊销/解绑之后，旧
// 连接名下的 cid 不应再投给新密钥；HTTP unpair 和吊销检测用它兜底。
func (b *Broker) CancelDevice(dev, reason string) int {
	if b.cmd == nil {
		return 0
	}
	return b.cmd.CancelDevice(dev, reason)
}

// KickDevice 断开这台设备的全部 MQTT 连接。
//
// 配对把密钥从 pending 转成 active 时，已有连接的身份表里还缓存着
// Pending=true；旧固件又只在 MQTT_EVENT_CONNECTED 时订阅 usage，
// 不踢的话它会一直停在配对页。断开后固件自动重连，这次用 active
// 身份，usage 订阅就能通过。
//
// 调用方必须先把 Verifier 缓存里的对应 KeyID 失效，否则重连可能
// 又拿到旧的 pending 身份。
func (b *Broker) KickDevice(dev string) {
	kicked := 0
	for id, cl := range b.srv.Clients.GetAll() {
		if cl.Net.Inline {
			continue
		}
		if id != dev && string(cl.Properties.Username) != dev {
			continue
		}
		if err := b.srv.DisconnectClient(cl, packets.CodeDisconnect); err == nil {
			kicked++
		}
	}
	if kicked > 0 {
		slog.Info("已断开设备 MQTT 连接", "dev", dev, "clients", kicked)
	}
}

// PublishFrame 通知设备有新帧。retain=0：版本是现场值，
// 设备重连时由 OnSubscribed 补发当前版本（见 frames.Service.Resend）。
func (b *Broker) PublishFrame(dev string, payload []byte) error {
	return b.srv.Publish(fmt.Sprintf("printer/%s/frame", dev), payload, false, 1)
}

// SetUsageResender 接上「设备订阅 usage 时补发」的回调。
// 用 setter 而不是构造参数：usage.Service 要用 broker 发消息，构造时还没有 broker。
func (b *Broker) SetUsageResender(r UsageResender) { b.hook.usage = r }

// SetFrameResender 接上「设备订阅 frame 时补发版本」的回调。
func (b *Broker) SetFrameResender(r FrameResender) { b.hook.frame = r }

// SetPairResender 接上「设备订阅 pair 时按密钥状态补发」的回调。
func (b *Broker) SetPairResender(r PairResender) { b.hook.pair = r }
