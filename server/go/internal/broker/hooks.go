package broker

import (
	"log/slog"
	"strings"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

// Router 是 broker 对在线状态表的窄依赖：收到设备心跳就记一笔。
type Router interface {
	Seen(dev string)
}

// UsageResender 在设备订阅 printer/{dev}/usage 时把最新用量补发给它。
type UsageResender interface {
	Resend(dev string)
}

type Hook struct {
	mqtt.HookBase
	v      *auth.Verifier
	router Router
	// ids 记录每个连接校验通过的身份，ACL 判定时直接取，不重复跑 argon2。
	ids *idTable
	// usage 为 nil 时不补发（测试里、或还没接上的时候）。
	usage UsageResender
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
	h.ids.put(cl, id)
	return true
}

func (h *Hook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	id, ok := h.ids.get(cl)
	if !ok {
		return false
	}
	return auth.ACLAllowed(id, topic, write)
}

// OnSubscribed：设备订阅自己的 usage 主题时，补发一份最新用量。
//
// usage 不用 retain（payload 里的 now 会过期），所以设备重连后要靠这里
// 才能立刻有数，否则得等到 agent 下一次上报（最长 10 分钟）。
// 在 goroutine 里发：hook 回调里同步 Publish 会和 broker 内部的订阅流程抢锁。
func (h *Hook) OnSubscribed(cl *mqtt.Client, pk packets.Packet, reasonCodes []byte) {
	id, ok := h.ids.get(cl)
	if !ok || id.Role != auth.RoleDevice {
		return
	}
	// 待配对密钥只能收 retained 的 pair；ACL 已经拒绝它订阅 usage，
	// 这里再挡一层，避免将来 ACL 回归时把用量补发出去。
	if id.Pending {
		return
	}
	want := "printer/" + id.Dev + "/usage"
	for i, f := range pk.Filters {
		if f.Filter != want || i >= len(reasonCodes) || reasonCodes[i] >= 0x80 {
			continue
		}
		if h.usage != nil {
			go h.usage.Resend(id.Dev)
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
