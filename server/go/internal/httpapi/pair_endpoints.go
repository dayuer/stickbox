package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/metrics"
)

// handleAgentPair：POST /api/agent/pair {code, name}
//
// 令牌可选：
//   - 不带令牌：新建一个无手机号的用户，设备归它，签发 agent 令牌；
//   - 带 agent 令牌：设备并入该用户（一个 agent 管多台屏）。
//
// 已绑定的 MAC 只有原主人的 agent 能转正；其他人输码返回 409。
func (a *API) handleAgentPair(w http.ResponseWriter, r *http.Request) {
	var agent *auth.Identity
	if tok, ok := bearerToken(r); ok {
		id, err := a.v.Verify(tok)
		if err != nil || id.Role != auth.RoleAgent {
			unauthorized(w)
			return
		}
		agent = &id
	}

	var in struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}

	ip := rateKeyIP(r)
	if a.pairFailIP.exceeded(ip) || a.pairFailWorld.exceeded("global") {
		fail(w, 429, "rate limited", "")
		return
	}
	failPair := func() {
		metrics.IncPairFail()
		a.pairFailIP.hit(ip)
		a.pairFailWorld.hit("global")
	}
	badCode := func(code int) {
		failPair()
		fail(w, code, "bad code", "")
	}

	if len(in.Code) != 6 {
		badCode(400)
		return
	}
	sum := sha256.Sum256([]byte(in.Code))
	hash := hex.EncodeToString(sum[:])
	// 先原子消费再验归属：两个请求同时带同一个码时只有一个能拿到行。
	pc, ok, err := a.store.ConsumePairCode(hash, time.Now().Unix())
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !ok {
		badCode(404)
		return
	}
	k, ok, err := a.store.KeyByID(pc.KeyID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !ok || k.Disabled || !k.Pending || k.Role != string(auth.RoleDevice) ||
		k.Dev != pc.Dev {
		badCode(409)
		return
	}

	owner, bound, err := a.store.OwnerOfDevice(pc.Dev)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}

	var userID, newToken string
	if agent != nil {
		if bound && owner != agent.UserID {
			a.store.RestorePairCode(pc) // 别人输错码不该把原主人的码烧掉
			failPair()
			fail(w, 409, "device bound", "该屏已绑定到其他用户")
			return
		}
		userID = agent.UserID
	} else {
		if bound {
			a.store.RestorePairCode(pc)
			failPair()
			fail(w, 409, "device bound",
				"该屏已绑定；请用原主人的 agent 配对")
			return
		}
		u, err := a.store.CreateUser()
		if err != nil {
			fail(w, 500, "server error", "")
			return
		}
		userID = u.ID
		name := in.Name
		if name == "" {
			name = "unknown"
		}
		if len(name) > 64 {
			name = name[:64]
		}
		newToken, err = auth.IssueAgentKey(a.v, a.store, userID, name)
		if err != nil {
			fail(w, 500, "server error", "")
			return
		}
	}

	activated, err := a.store.ActivateDeviceKey(pc.KeyID, userID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !activated {
		// 并发请求已经把同一把待配对密钥转正/吊销了。
		badCode(409)
		return
	}
	// 旧主人的密钥作废，刚转正的那把留着。
	if err := auth.RevokeDeviceKeysExcept(a.store, a.v, pc.Dev, pc.KeyID); err != nil {
		fail(w, 500, "server error", "")
		return
	}
	a.v.Invalidate(pc.KeyID) // 缓存里那把还是 UserID="" 的待配对身份
	// 旧密钥名下的未确认指令不能投给新密钥：设备换 key_id 了，旧 cid
	// 对它没有意义。先作废，再发新的 pair/usage。
	if n := a.pub.CancelDevice(pc.Dev, "key_replaced"); n > 0 {
		slog.Info("换密钥后作废旧指令", "dev", pc.Dev, "count", n)
	}

	// 带上 key_id：设备只认发给「自己这把密钥」的通知，
	// 避免上一轮配对 retain 的消息被下一轮注册误读。
	pairMsg := fmt.Sprintf(`{"paired":true,"key_id":"%s"}`, pc.KeyID)
	a.pairMu.Lock()
	a.publishPairLocked(pc.Dev, []byte(pairMsg), "true:"+pc.KeyID)
	a.pairMu.Unlock()
	// retained 的 pair 已存进 broker。现在踢掉旧连接：旧固件只在
	// MQTT_EVENT_CONNECTED 时订阅 usage，而这条连接的身份还是 pending。
	// 重连后身份是 active，usage 订阅就能通过，也能收到 retained 的 paired=true。
	a.pub.KickDevice(pc.Dev)
	// 配对成功立即补发最新用量；设备这时才第一次拿到主人的数据。
	a.usage.Resend(pc.Dev)
	// 新屏可能马上就要取帧：标脏让后台 worker 出一份
	// （没有用量就是「等待上报」帧）。
	a.markFramesDirty(userID)

	metrics.IncPairOK()
	writeJSON(w, 200, map[string]any{
		"dev": pc.Dev, "user_id": userID, "token": newToken,
	})
}

// ResendPair 实现 broker.PairResender：设备订阅 printer/{dev}/pair 时，
// 按这条连接自己那把密钥的当前状态补发一条配对通知。
//
// retained 的 pair 只存在 broker 内存里，服务端一重启就没了。新固件要
// 收到 paired=true 才订阅 usage/frame，只靠 retained 会永远卡在配对页。
// 这里按 KeyID 查库，和 usage/frame 的 Resend 一样，不依赖 retained。
func (a *API) ResendPair(dev, keyID string) {
	// 持锁查库 + 发布：和配对/解绑的发布串行，避免补发的 true 压过
	// 随后到达的 false（解绑后设备还误以为已配对）。
	a.pairMu.Lock()
	defer a.pairMu.Unlock()

	k, ok, err := a.store.KeyByID(keyID)
	if err != nil {
		slog.Warn("补发配对状态失败", "dev", dev, "key_id", keyID, "err", err)
		return
	}
	var payload []byte
	var state string
	switch {
	case !ok || k.Disabled:
		// 密钥已吊销（或库里已不存在）：让设备清掉密钥重新注册。
		payload, _ = json.Marshal(map[string]any{
			"paired": false, "key_ids": []string{keyID},
		})
		state = "false"
	case k.Pending:
		// 还没配对，继续等 agent 输码。
		return
	case k.Role != string(auth.RoleDevice) || k.Dev != dev:
		// 身份与订阅主题不一致，防御性忽略。
		return
	default:
		payload = []byte(fmt.Sprintf(`{"paired":true,"key_id":"%s"}`, k.KeyID))
		state = "true:" + k.KeyID
	}
	// 本进程已经发过同样的状态：broker 的 retained 还在，不用重复发。
	if a.pairSent[dev] == state {
		return
	}
	a.publishPairLocked(dev, payload, state)
}

// publishPairLocked 发布并记录配对状态；调用方必须持有 a.pairMu。
func (a *API) publishPairLocked(dev string, payload []byte, state string) {
	_ = a.pub.PublishPair(dev, payload)
	if a.pairSent == nil {
		a.pairSent = map[string]string{}
	}
	a.pairSent[dev] = state
}

// handleAgentUnpair：POST /api/agent/unpair {dev}
//
// 只能解绑自己名下的屏。设备收到 pair=false 后重新注册出码。
func (a *API) handleAgentUnpair(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	var in struct {
		Dev string `json:"dev"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !a.ownsDevice(id.UserID, in.Dev) {
		fail(w, 403, "forbidden", "")
		return
	}
	revoked, err := a.store.DisableKeysOfDevice(in.Dev, string(auth.RoleDevice))
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	for _, keyID := range revoked {
		a.v.Invalidate(keyID)
	}
	a.store.DeletePairCode(in.Dev)
	// retain 的 false 带上被吊销的 key_id 列表；设备只在自己的密钥
	// 出现在列表里时才重启，重新注册的新密钥不会被旧通知误伤。
	msg, _ := json.Marshal(map[string]any{
		"paired": false, "key_ids": revoked,
	})
	a.pairMu.Lock()
	a.publishPairLocked(in.Dev, msg, "false")
	a.pairMu.Unlock()
	// 解绑后旧密钥已失效，连接不应该继续挂着。先让 pair=false 送达
	// （设备要清掉密钥并重新注册），再踢；直接踢会丢掉这条通知。
	// 设备换密钥/重新注册后，旧连接名下的 usage/frame cid 不应再重投；
	// pair 本身已经是 noAck + retained，不在这里处理。
	if n := a.pub.CancelDevice(in.Dev, "unpaired"); n > 0 {
		slog.Info("解绑后作废旧指令", "dev", in.Dev, "count", n)
	}
	go func(dev string) {
		time.Sleep(300 * time.Millisecond)
		a.pub.KickDevice(dev)
	}(in.Dev)
	// 解绑不重渲染；名下已无设备时由帧服务清掉缓存（见 DeviceRemoved）。
	if a.frames != nil {
		a.frames.DeviceRemoved(id.UserID)
	}
	writeJSON(w, 200, map[string]int{"ok": 1})
}

// handleAgentDevices：GET /api/agent/devices
func (a *API) handleAgentDevices(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	devs, err := a.store.DevicesOfUser(id.UserID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	type row struct {
		Dev    string `json:"dev"`
		Name   string `json:"name"`
		Online bool   `json:"online"`
		Bound  int64  `json:"bound"`
	}
	out := []row{}
	for _, dev := range devs {
		it := row{Dev: dev, Online: a.reg.IsOnline(dev)}
		if meta, ok, err := a.store.DeviceMeta(dev); err == nil && ok {
			it.Name = meta.Name
			it.Bound = meta.Bound
		}
		out = append(out, it)
	}
	writeJSON(w, 200, map[string]any{"devices": out})
}
