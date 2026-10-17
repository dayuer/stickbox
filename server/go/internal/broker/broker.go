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

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

// CommandTracker 是 broker 对下行确认跟踪的窄依赖（见 internal/command）。
type CommandTracker interface {
	Send(dev, topic string, payload []byte, retain bool) (string, error)
	Ack(dev, cid string, ok bool, errMsg string)
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
	srv  *mqtt.Server
	hook *Hook
	cmd  CommandTracker
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
	h := &Hook{v: v, router: r, ids: newIDTable()}
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

func (b *Broker) Serve() error { return b.srv.Serve() }
func (b *Broker) Close() error { return b.srv.Close() }

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
// retain 和 T18 的重投是两件事：retain 解决「重连能拿到最后状态」，
// 重投解决「收到了但没应用」。两者都要。
func (b *Broker) PublishPair(dev string, payload []byte) error {
	return b.send(dev, fmt.Sprintf("printer/%s/pair", dev), payload, true)
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

// SetUsageResender 接上「设备订阅 usage 时补发」的回调。
// 用 setter 而不是构造参数：usage.Service 要用 broker 发消息，构造时还没有 broker。
func (b *Broker) SetUsageResender(r UsageResender) { b.hook.usage = r }
