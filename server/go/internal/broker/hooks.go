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

// Claimer 是 broker 对 store 的窄依赖：把待认领的密钥绑到设备上。
type Claimer interface {
	ClaimKey(keyID, dev string) (bool, error)
	OwnerOfDevice(dev string) (string, bool, error)
}

type Hook struct {
	mqtt.HookBase
	v      *auth.Verifier
	mem    *Membership
	router Router
	claim  Claimer
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

	// agent 只走 HTTP 上报，ACL 也不给它任何 topic。连接直接拒掉，
	// 省得它占着一条连接什么都干不了。
	if id.Role == auth.RoleAgent {
		slog.Warn("agent 令牌不允许连 MQTT", "username", user)
		return false
	}

	// 待认领的密钥：App 在还没连上设备热点时就签发了它，那时候还不知道 MAC。
	// 设备现在自己报上来了——username 就是它的 dev。
	if id.Role == auth.RoleDevice && id.Dev == "" {
		claimed, ok := h.claimKey(id, user)
		if !ok {
			return false
		}
		id = claimed
	}

	// device 角色的 username 必须等于它自己的 dev——防止拿 A 的密钥冒充 B 连接。
	if id.Role == auth.RoleDevice && id.Dev != user {
		slog.Warn("MQTT username 与密钥不符", "username", user, "key_dev", id.Dev)
		return false
	}
	h.ids.put(cl, id)
	return true
}

// claimKey 把一把待认领的密钥绑到设备自报的 dev 上。
//
// 抢绑防护从 enroll 挪到了这里：App 不带 dev 签发密钥时，服务端那一刻还
// 不知道要绑哪台，所以只能在设备真的连上来时才判。判据不变——这台设备
// 已经属于别人就拒绝，不然任何人把自己的密钥写进别人的设备就能顶掉绑定。
func (h *Hook) claimKey(id auth.Identity, dev string) (auth.Identity, bool) {
	if !auth.ValidDev(dev) {
		slog.Warn("待认领密钥的 username 不是合法 dev", "username", dev)
		return id, false
	}
	if h.claim == nil {
		slog.Error("没有 Claimer，待认领的密钥无法绑定", "key_id", id.KeyID)
		return id, false
	}
	if owner, bound, err := h.claim.OwnerOfDevice(dev); err != nil {
		slog.Error("查设备归属失败", "dev", dev, "err", err)
		return id, false
	} else if bound && owner != id.UserID {
		slog.Warn("拒绝认领：设备属于其他账号", "dev", dev)
		return id, false
	}
	ok, err := h.claim.ClaimKey(id.KeyID, dev)
	if err != nil {
		slog.Error("认领密钥失败", "dev", dev, "err", err)
		return id, false
	}
	if !ok {
		// 已经被别的设备抢先认领了。并发保护在 SQL 的 dev='' 条件里。
		slog.Warn("密钥已被认领，拒绝", "dev", dev, "key_id", id.KeyID)
		return id, false
	}
	// 缓存里那份 Identity 的 Dev 还是空的，不清掉后面的 ACL 会全部拒绝。
	h.v.Invalidate(id.KeyID)
	h.mem.Invalidate(id.UserID)
	id.Dev = dev
	slog.Info("设备已认领密钥", "dev", dev, "user", id.UserID)
	return id, true
}

func (h *Hook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	id, ok := h.ids.get(cl)
	if !ok {
		return false
	}
	return auth.ACLAllowed(id, topic, write, h.mem)
}

// OnSubscribed：设备订阅自己的 usage 主题时，补发一份最新用量。
//
// usage 不用 retain（payload 里的 now 会过期），所以设备重连后要靠这里
// 才能立刻有数，否则得等到 agent 下一次上报（最长 10 分钟）。
// 在 goroutine 里发：hook 回调里同步 Publish 会和 broker 内部的订阅流程抢锁。
func (h *Hook) OnSubscribed(cl *mqtt.Client, pk packets.Packet, reasonCodes []byte) {
	if h.usage == nil {
		return
	}
	id, ok := h.ids.get(cl)
	if !ok || id.Role != auth.RoleDevice {
		return
	}
	want := "printer/" + id.Dev + "/usage"
	for i, f := range pk.Filters {
		if f.Filter != want || i >= len(reasonCodes) || reasonCodes[i] >= 0x80 {
			continue
		}
		go h.usage.Resend(id.Dev)
	}
}

func (h *Hook) OnDisconnect(cl *mqtt.Client, err error, expire bool) {
	h.ids.drop(cl)
}

// OnPublished：设备往自己的 status 主题发心跳，记一笔在线。
//
// topic 是经过 ACL 校验的，dev 一律从 topic 取；payload 内容不参与判断——
// 设备在打印退役后只用心跳维持「在线」这一个状态，作业/打印机字段都已删除。
func (h *Hook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	if h.router == nil {
		return
	}
	parts := strings.Split(pk.TopicName, "/")
	// 主题固定是三段：printer/{dev}/status。不做通配符展开，
	// 也不接受畸形 dev——否则 "printer/+/status" 会把通配符记成在线设备。
	if len(parts) != 3 || parts[0] != "printer" || parts[2] != "status" {
		return
	}
	if !auth.ValidDev(parts[1]) {
		return
	}
	h.router.Seen(parts[1])
}
