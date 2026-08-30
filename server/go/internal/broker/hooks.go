package broker

import (
	"encoding/json"
	"log/slog"
	"strings"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/device"
)

// Router 是 broker 对 registry 的窄依赖。
type Router interface {
	Send(dev string, m device.Msg)
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
}

func (h *Hook) ID() string { return "stickbox-auth" }

func (h *Hook) Provides(b byte) bool {
	switch b {
	case mqtt.OnConnectAuthenticate, mqtt.OnACLCheck, mqtt.OnPublished, mqtt.OnDisconnect:
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

func (h *Hook) OnDisconnect(cl *mqtt.Client, err error, expire bool) {
	h.ids.drop(cl)
}

func (h *Hook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	if strings.HasSuffix(pk.TopicName, "/status") {
		routeStatus(h.router, pk.TopicName, pk.Payload)
	}
}

// statusPayload 是设备心跳的结构。字段定义见 docs/API-cloud-print.md 第 3.6 节。
type statusPayload struct {
	Job    string          `json:"job"`
	State  string          `json:"state"`
	Bytes  int64           `json:"bytes"`
	Err    string          `json:"err"`
	Serial string          `json:"serial"`
	Prn    json.RawMessage `json:"prn"`
	// USB 事件计数（API 文档 3.6 的 usb 字段）。设备侧那个「拔掉能认出来、
	// 插回去一直未连接」的故障在设备上没有任何可见输出，只能靠它诊断。
	USB json.RawMessage `json:"usb"`
	// Heap 是设备的空闲堆字节数。设备一直在报，之前被丢掉了——结果堆耗尽
	// 引起的 TLS 握手失败在服务端只表现为「证书错」，查不出真因。
	Heap uint32 `json:"heap"`
}

// routeStatus 把一条 status 消息翻译成 actor 消息。
//
// dev 一律从 topic 取，不用 payload 里的——payload 是设备自报的，
// topic 是经过 ACL 校验的，只有后者可信。
func routeStatus(r Router, topic string, payload []byte) {
	parts := strings.Split(topic, "/")
	if len(parts) != 3 || parts[0] != "printer" {
		return
	}
	dev := parts[1]

	var p statusPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		slog.Warn("status payload 非法 JSON", "dev", dev)
		return
	}

	// USB 计数原样带给 actor，/api/status 会把它摊出来。
	//
	// 只在异常时打日志是不够的：那类故障在设备上没有任何可见输出，
	// 排查时需要的是「现在的计数是多少」，而不是「出事那一刻打了条日志」。
	if len(p.USB) > 0 {
		var u struct {
			Att uint32 `json:"att"`
			Det uint32 `json:"det"`
			Err string `json:"err"`
		}
		if json.Unmarshal(p.USB, &u) == nil {
			if u.Err != "" {
				slog.Error("设备 USB 清理失败——插回打印机可能认不出来",
					"dev", dev, "err", u.Err, "att", u.Att, "det", u.Det)
			} else if u.Det > 0 && u.Att < u.Det {
				slog.Warn("设备有一次插回后未枚举成功",
					"dev", dev, "att", u.Att, "det", u.Det)
			}
		}
	}

	m := device.Msg{Serial: p.Serial, Printer: p.Prn, JobID: p.Job,
		Bytes: p.Bytes, Err: p.Err, USB: p.USB, Heap: p.Heap}
	switch p.State {
	case "done":
		m.Kind = device.KindJobDone
	case "failed":
		m.Kind = device.KindJobFailed
	case "downloading":
		m.Kind = device.KindDownloading
	case "offline":
		m.Kind = device.KindHeartbeat
		m.Serial = "" // 掉线了就没有打印机
	default:
		m.Kind = device.KindHeartbeat
	}
	r.Send(dev, m)
}
