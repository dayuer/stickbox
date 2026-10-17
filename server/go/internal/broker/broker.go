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

// SetUsageResender 接上「设备订阅 usage 时补发」的回调。
// 用 setter 而不是构造参数：usage.Service 要用 broker 发消息，构造时还没有 broker。
func (b *Broker) SetUsageResender(r UsageResender) { b.hook.usage = r }
