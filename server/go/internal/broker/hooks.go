package broker

import (
	"log/slog"
	"strings"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/metrics"
)

// Router 是 broker 对在线状态表的窄依赖：收到设备心跳就记一笔。
type Router interface {
	Seen(dev string)
}

// UsageResender 在设备订阅 printer/{dev}/usage 时把最新用量补发给它。
type UsageResender interface {
	Resend(dev string)
}

// FrameResender 在设备订阅 printer/{dev}/frame 时补发当前帧版本。
type FrameResender interface {
	Resend(dev string)
}

type Hook struct {
	mqtt.HookBase
	srv    *mqtt.Server
	v      *auth.Verifier
	router Router
	// ids 记录每个连接校验通过的身份，ACL 判定时直接取，不重复跑 argon2。
	ids *idTable
	// usage / frame 为 nil 时不补发（测试里、或还没接上的时候）。
	usage UsageResender
	frame FrameResender
	// pending 限制同一 dev 的 pending 连接频率。
	pending *pendingLimiter
}

func (h *Hook) ID() string { return "stickbox-auth" }

func (h *Hook) Provides(b byte) bool {
	switch b {
	case mqtt.OnConnectAuthenticate, mqtt.OnACLCheck, mqtt.OnPublished, mqtt.OnDisconnect,
		mqtt.OnSubscribed:
		return true
	}
	return false
}

func (h *Hook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	user := string(pk.Connect.Username)
	id, err := h.v.Verify(string(pk.Connect.Password))
	if err != nil {
		slog.Warn("MQTT 认证失败", "username", user, "remote", cl.Net.Remote)
		return false
	}

	// agent 只走 HTTP，ACL 也不给它任何 topic。
	if id.Role == auth.RoleAgent {
		slog.Warn("agent 令牌不允许连 MQTT", "username", user)
		return false
	}

	// T7 起设备注册时就直接带 dev，不会再签出 dev='' 的密钥。
	// 旧库里若还有这种密钥，直接拒绝，让设备重新注册出码。
	if id.Role == auth.RoleDevice && id.Dev == "" {
		slog.Warn("设备密钥未绑定 dev，拒绝连接；请重新注册", "key_id", id.KeyID)
		return false
	}
	// device 角色的 username 必须等于它自己的 dev——防止拿 A 的密钥冒充 B 连接。
	if id.Role == auth.RoleDevice && id.Dev != user {
		slog.Warn("MQTT username 与密钥不符", "username", user, "key_dev", id.Dev)
		return false
	}
	// pending 密钥只能读 pair；它不能把已在线 active 设备顶下线。
	// 同一个 dev 的 pending 连接还要限频，防止趁合法设备重连空档抢先。
	if id.Role == auth.RoleDevice && id.Pending {
		if h.pending != nil && !h.pending.allow(id.Dev, time.Now()) {
			slog.Warn("pending 连接过频，拒绝", "dev", id.Dev, "remote", cl.Net.Remote)
			return false
		}
		if !h.ids.admit(cl, id) {
			metrics.IncPendingTakeoverRejected()
			slog.Warn("pending 连接试图顶掉在线设备", "dev", id.Dev, "remote", cl.Net.Remote)
			return false
		}
		h.evictStale(id.Dev)
		return true
	}
	if !h.ids.admit(cl, id) {
		return false
	}
	h.evictStale(id.Dev)
	return true
}

// evictStale 在 mochi 走会话接管之前，把同 client ID 的旧连接踢掉并等它
// 从 Clients 里消失。
//
// mochi v2.7.9 的 inheritClientSession 有竞态：被顶掉的旧连接可能在
// isTakenOver 置位之前进入清理流程，于是它在 Clients.Add(新连接) 之后
// 仍会按 client ID 执行 Clients.Delete，把新连接从路由表里删掉。表现是
// 连接没断、订阅也 SUBACK 了，但之后所有消息都送不到，直到设备重连。
// 先同步踢掉旧连接并等它清完，新连接就不会再走接管路径。
func (h *Hook) evictStale(dev string) {
	if h.srv == nil {
		return
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		old, ok := h.srv.Clients.Get(dev)
		if !ok || old.Net.Inline {
			return
		}
		// 非 clean 会话断线后 mochi 不会删 Clients 条目，不存在上面的
		// 竞态；接管时它也会保留订阅，直接交给 mochi 处理。
		if !sessionExpiresOnDisconnect(old) {
			return
		}
		_ = h.srv.DisconnectClient(old, packets.ErrSessionTakenOver)
		if !time.Now().Before(deadline) {
			slog.Warn("旧连接未在期限内退出", "dev", dev)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sessionExpiresOnDisconnect 与 mochi 清理连接时的 expire 判断一致。
func sessionExpiresOnDisconnect(cl *mqtt.Client) bool {
	if cl.Properties.ProtocolVersion == 5 {
		return cl.Properties.Props.SessionExpiryInterval == 0
	}
	return cl.Properties.Clean
}

func (h *Hook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	id, ok := h.ids.get(cl)
	if !ok {
		return false
	}
	return auth.ACLAllowed(id, topic, write)
}

// OnSubscribed：设备订阅自己的 usage / frame 主题时，补发当前值。
//
// 两者都不用 retain（用量里的 now 会过期、帧版本也会变），设备重连后
// 要靠这里立刻拿到状态，否则得等下一次上报或重建。
// 在 goroutine 里发：hook 回调里同步 Publish 会和 broker 内部的订阅流程抢锁。
func (h *Hook) OnSubscribed(cl *mqtt.Client, pk packets.Packet, reasonCodes []byte) {
	id, ok := h.ids.get(cl)
	if !ok || id.Role != auth.RoleDevice {
		return
	}
	// 待配对密钥只能收 retained 的 pair；ACL 已经拒绝它订阅 usage/frame，
	// 这里再挡一层，避免将来 ACL 回归时把数据补发出去。
	if id.Pending {
		return
	}
	for i, f := range pk.Filters {
		if i >= len(reasonCodes) || reasonCodes[i] >= 0x80 {
			continue
		}
		switch f.Filter {
		case "printer/" + id.Dev + "/usage":
			if h.usage != nil {
				go h.usage.Resend(id.Dev)
			}
		case "printer/" + id.Dev + "/frame":
			if h.frame != nil {
				go h.frame.Resend(id.Dev)
			}
		}
	}
}

func (h *Hook) OnDisconnect(cl *mqtt.Client, err error, expire bool) {
	h.ids.drop(cl)
}

// OnPublished：设备往自己的 status 主题发心跳，记一笔在线。
//
// topic 是经过 ACL 校验的，dev 一律从 topic 取；payload 内容不参与判断。
func (h *Hook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	if h.router == nil {
		return
	}
	parts := strings.Split(pk.TopicName, "/")
	if len(parts) != 3 || parts[0] != "printer" || parts[2] != "status" {
		return
	}
	if !auth.ValidDev(parts[1]) {
		return
	}
	h.router.Seen(parts[1])
}
