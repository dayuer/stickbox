package broker

import (
	"encoding/json"
	"errors"
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

// PairResender 在设备订阅 printer/{dev}/pair 时，按这条连接自己那把
// 密钥的当前状态补发一条配对通知。retained 消息只在 broker 内存里，
// 服务端重启后就没了，不能只依赖它。
type PairResender interface {
	ResendPair(dev, keyID string)
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
	pair  PairResender
	// cmd 为 nil 时不处理 ACK（同上）。
	cmd CommandTracker
	// pending 限制同一 dev 的 pending 连接频率。
	pending *pendingLimiter
	// auth 是最近 CONNECT 结果环形缓冲，device show 诊断用。
	auth *authLog
	// admit 是全局新连接准入（每秒）。
	admit *admission
}

func (h *Hook) ID() string { return "stickbox-auth" }

func (h *Hook) Provides(b byte) bool {
	switch b {
	case mqtt.OnConnect, mqtt.OnConnectAuthenticate, mqtt.OnACLCheck,
		mqtt.OnPublished, mqtt.OnPublishDropped, mqtt.OnQosDropped,
		mqtt.OnDisconnect, mqtt.OnSubscribed:
		return true
	}
	return false
}

// OnConnect 做准入 + 认证 + 身份登记。
//
// 认证原本在 OnConnectAuthenticate 里，但那个钩子只能返回 bool，mochi 会
// 统一回 CONNACK 0x05（未授权）。服务端繁忙/限流/数据库错误也回 0x05 的话，
// 固件会以为密钥被吊销而清 NVS、重新注册——攻击者只要打满连接就能逐台
// “解绑”设备。所以把可能返回 0x03（server unavailable）的判断搬到
// OnConnect，只有真正的密钥/身份问题才留给 OnConnectAuthenticate 回 0x05。
func (h *Hook) OnConnect(cl *mqtt.Client, pk packets.Packet) error {
	if h.admit != nil && !h.admit.allow(cl.Net.Remote) {
		slog.Warn("MQTT 连接被准入限流", "remote", cl.Net.Remote)
		h.noteAuth(cl, string(pk.Connect.Username), 0x03, "admission")
		return packets.ErrServerUnavailable // 0x03：让设备退避重试，不要清密钥
	}
	user := string(pk.Connect.Username)
	id, err := h.v.Verify(string(pk.Connect.Password))
	if err != nil {
		if errors.Is(err, auth.ErrDenied) {
			// 真·密钥不对：让 OnConnectAuthenticate 回 0x05。
			slog.Warn("MQTT 认证失败", "username", user, "remote", cl.Net.Remote)
			h.noteAuth(cl, user, 0x05, "invalid_key")
			return nil
		}
		// 查库失败等临时错误：0x03，不能告诉设备密钥无效。
		slog.Warn("MQTT 认证暂时不可用", "username", user, "remote", cl.Net.Remote, "err", err)
		h.noteAuth(cl, user, 0x03, "db_error")
		return packets.ErrServerUnavailable
	}

	// 非设备令牌（agent / user）只走 HTTP，ACL 也不给它们任何 topic。
	if id.Role != auth.RoleDevice {
		slog.Warn("非设备令牌不允许连 MQTT", "username", user, "role", id.Role)
		h.noteAuth(cl, user, 0x05, "role")
		return nil
	}

	// T7 起设备注册时就直接带 dev，不会再签出 dev='' 的密钥。
	if id.Role == auth.RoleDevice && id.Dev == "" {
		slog.Warn("设备密钥未绑定 dev，拒绝连接；请重新注册", "key_id", id.KeyID)
		h.noteAuth(cl, user, 0x05, "no_dev")
		return nil
	}
	// device 角色的 username 必须等于它自己的 dev——防止拿 A 的密钥冒充 B 连接。
	if id.Role == auth.RoleDevice && id.Dev != user {
		slog.Warn("MQTT username 与密钥不符", "username", user, "key_dev", id.Dev)
		h.noteAuth(cl, user, 0x05, "dev_mismatch")
		return nil
	}
	// pending 密钥只能读 pair；它不能把已在线 active 设备顶下线。
	if id.Role == auth.RoleDevice && id.Pending {
		if h.pending != nil && !h.pending.allow(id.Dev, time.Now()) {
			// 限频不是“密钥无效”，回 0x03 让设备退避。
			slog.Warn("pending 连接过频，稍后重试", "dev", id.Dev, "remote", cl.Net.Remote)
			h.noteAuth(cl, user, 0x03, "pending_rate")
			return packets.ErrServerUnavailable
		}
		// CLI 的 device unbind/revoke 直接改库，不和 broker 同进程，没法
		// 主动踢线。先清掉已经失效的在线身份，否则那条幽灵连接会把新
		// pending 连接一直挡在门外（设备表现为清密钥→重启→再注册的死循环）。
		h.dropRevoked(id.Dev)
		if !h.ids.admit(cl, id) {
			metrics.IncPendingTakeoverRejected()
			// 已有一台 active 在线：这是“稍后再试”，不是密钥被吊销。
			slog.Warn("pending 连接试图顶掉在线设备，稍后重试",
				"dev", id.Dev, "remote", cl.Net.Remote)
			h.noteAuth(cl, user, 0x03, "pending_takeover")
			return packets.ErrServerUnavailable
		}
		h.evictStale(cl.ID)
		h.noteAuth(cl, user, 0x00, "ok_pending")
		return nil
	}
	if !h.ids.admit(cl, id) {
		slog.Warn("连接身份登记失败，稍后重试", "remote", cl.Net.Remote)
		h.noteAuth(cl, user, 0x03, "admit_failed")
		return packets.ErrServerUnavailable
	}
	h.evictStale(cl.ID)
	h.noteAuth(cl, user, 0x00, "ok")
	return nil
}

// OnConnectAuthenticate 只回答“OnConnect 是否已经放行并登记了这条连接”。
func (h *Hook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	_, ok := h.ids.get(cl)
	return ok
}

func (h *Hook) OnPublishDropped(cl *mqtt.Client, pk packets.Packet) {
	metrics.IncMQTTPublishDropped()
}

func (h *Hook) OnQosDropped(cl *mqtt.Client, pk packets.Packet) {
	metrics.IncMQTTQosDropped()
}

// dropRevoked 把同 dev、非 pending、但密钥已在库里被吊销/禁用的连接
// 从身份表移除并断开。查询失败（DB 繁忙等）不当成吊销，保留连接。
func (h *Hook) dropRevoked(dev string) {
	if h.v == nil {
		return
	}
	stale := h.ids.dropWhere(func(id auth.Identity) bool {
		if id.Role != auth.RoleDevice || id.Dev != dev || id.Pending {
			return false
		}
		_, valid, err := h.v.Lookup(id.KeyID)
		return err == nil && !valid
	})
	for _, cl := range stale {
		slog.Warn("密钥已吊销，断开在线连接", "dev", dev)
		if h.srv != nil {
			_ = h.srv.DisconnectClient(cl, packets.ErrNotAuthorized)
		}
	}
	// 吊销后设备已经不能以原身份回 ACK，pair 指令（解绑通知）再重投也没有
	// 意义；直接结案，别把重试预算耗在一条不会有人确认的指令上。
	if len(stale) > 0 && h.cmd != nil {
		if n := h.cmd.CancelDevice(dev, "device_revoked"); n > 0 {
			slog.Info("吊销后作废旧指令", "dev", dev, "count", n)
		}
	}
}

// evictStale 在 mochi 走会话接管之前，把**同一个 client ID** 的旧连接
// 踢掉并等它从 Clients 里消失。不能按 dev 找：pending 密钥用不同的
// client ID 连上来时，按 dev 找会误踢在线的合法设备。
//
// mochi v2.7.9 的 inheritClientSession 有竞态：被顶掉的旧连接可能在
// isTakenOver 置位之前进入清理流程，于是它在 Clients.Add(新连接) 之后
// 仍会按 client ID 执行 Clients.Delete，把新连接从路由表里删掉。表现是
// 连接没断、订阅也 SUBACK 了，但之后所有消息都送不到，直到设备重连。
// 先同步踢掉旧连接并等它清完，新连接就不会再走接管路径。
func (h *Hook) evictStale(clientID string) {
	if h.srv == nil {
		return
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		old, ok := h.srv.Clients.Get(clientID)
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
			slog.Warn("旧连接未在期限内退出", "client", clientID)
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

// OnSubscribed：设备订阅自己的 pair / usage / frame 主题时，补发当前值。
//
// usage / frame 不用 retain（用量里的 now 会过期、帧版本也会变）；pair
// 虽然有 retained，但那只存在 broker 内存里，重启就没了，所以也要按
// 密钥的当前状态补发，否则已配对设备会一直等不到 paired=true。
// 在 goroutine 里发：hook 回调里同步 Publish 会和 broker 内部的订阅流程抢锁。
func (h *Hook) OnSubscribed(cl *mqtt.Client, pk packets.Packet, reasonCodes []byte) {
	id, ok := h.ids.get(cl)
	if !ok || id.Role != auth.RoleDevice {
		return
	}
	// 待配对密钥只能收 pair；ACL 已经拒绝它订阅 usage/frame，
	// 这里再挡一层，避免将来 ACL 回归时把数据补发出去。
	pending := id.Pending
	for i, f := range pk.Filters {
		if i >= len(reasonCodes) || reasonCodes[i] >= 0x80 {
			continue
		}
		switch f.Filter {
		case "printer/" + id.Dev + "/pair":
			if h.pair != nil {
				go h.pair.ResendPair(id.Dev, id.KeyID)
			}
		case "printer/" + id.Dev + "/usage":
			if !pending && h.usage != nil {
				go h.usage.Resend(id.Dev)
			}
		case "printer/" + id.Dev + "/frame":
			if !pending && h.frame != nil {
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
	// 心跳时实时核对密钥：CLI 吊销不会踢线，已吊销的密钥不能继续
	// 收用量/帧，发现即断开，让设备重新认证并走注册流程。
	if h.ids != nil && h.v != nil {
		if id, ok := h.ids.get(cl); ok && id.Role == auth.RoleDevice {
			if _, valid, err := h.v.Lookup(id.KeyID); err == nil && !valid {
				slog.Warn("密钥已吊销，断开连接", "dev", id.Dev, "key_id", id.KeyID)
				if h.srv != nil {
					_ = h.srv.DisconnectClient(cl, packets.ErrNotAuthorized)
				}
				return
			}
		}
	}
	parts := strings.Split(pk.TopicName, "/")
	if len(parts) != 3 || parts[0] != "printer" || !auth.ValidDev(parts[1]) {
		return
	}
	switch parts[2] {
	case "status":
		if h.router != nil {
			h.router.Seen(parts[1])
		}
	case "ack":
		h.onAck(parts[1], pk.Payload)
	}
}

// onAck 处理设备回执。设备说 ok=0 表示「收到了但刻意不应用」，那是永久
// 失败，tracker 会立即结案而不是继续重投同样会失败的东西。
func (h *Hook) onAck(dev string, payload []byte) {
	if h.cmd == nil {
		return
	}
	var a struct {
		CID string `json:"cid"`
		OK  int    `json:"ok"`
		Err string `json:"e"`
	}
	if err := json.Unmarshal(payload, &a); err != nil || a.CID == "" {
		slog.Warn("ACK 不是合法 JSON 或缺 cid", "dev", dev)
		return
	}
	h.cmd.Ack(dev, a.CID, a.OK == 1, a.Err)
}
