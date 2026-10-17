// Package broker 把 MQTT broker 塞进本进程。
//
// 内嵌之后推一条用量是一次内存中的函数调用，不再经网络。
// 认证和 ACL 也跟 HTTP 共用同一套密钥，不用维护第二份配置。
package broker

import (
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
// pending 身份不能顶掉已在线 active 连接；active 可以接管 pending
// （合法设备重新上线时这是正常的）。
func (t *idTable) admit(cl *mqtt.Client, id auth.Identity) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id.Role == auth.RoleDevice && id.Pending {
		for _, cur := range t.m {
			if cur.Role == auth.RoleDevice && cur.Dev == id.Dev && !cur.Pending {
				return false
			}
		}
	}
	t.m[cl] = id
	return true
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

type Broker struct {
	srv     *mqtt.Server
	hook    *Hook
	healthy atomic.Bool
}

func New(addr string, tlsCfg *tls.Config, v *auth.Verifier, r Router) (*Broker, error) {
	srv := mqtt.New(&mqtt.Options{
		// 服务端要能直接 Publish 推用量，这个开关是前提；
		// 不开的话 Publish 返回「please set Options.InlineClient=true」。
		InlineClient: true,
		// broker 自己的日志走 slog，别另起一套格式
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelWarn,
		})),
	})
	h := &Hook{
		srv: srv, v: v, router: r, ids: newIDTable(),
		pending: newPendingLimiter(6, time.Minute),
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

// PublishUsage 推 Claude 用量。retain=0：payload 里的 now 是现场盖的，
// 留着会过期；设备重连时由 OnSubscribed 触发补发一份新的（见 usage 包）。
func (b *Broker) PublishUsage(dev string, payload []byte) error {
	return b.srv.Publish(fmt.Sprintf("printer/%s/usage", dev), payload, false, 1)
}

// PublishPair 通知设备配对状态。retain=1：设备离线时错过配对，
// 重连后仍能拿到「已配对 / 已解绑」的当前状态。
func (b *Broker) PublishPair(dev string, payload []byte) error {
	return b.srv.Publish(fmt.Sprintf("printer/%s/pair", dev), payload, true, 1)
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
