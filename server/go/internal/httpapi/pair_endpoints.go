package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
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
		newToken, err = auth.IssueAgentKey(a.store, userID, name)
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

	// 带上 key_id：设备只认发给「自己这把密钥」的通知，
	// 避免上一轮配对 retain 的消息被下一轮注册误读。
	pairMsg := fmt.Sprintf(`{"paired":true,"key_id":"%s"}`, pc.KeyID)
	a.pub.PublishPair(pc.Dev, []byte(pairMsg))
	// 配对成功立即补发最新用量；设备这时才第一次拿到主人的数据。
	a.usage.Resend(pc.Dev)

	writeJSON(w, 200, map[string]any{
		"dev": pc.Dev, "user_id": userID, "token": newToken,
	})
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
	a.pub.PublishPair(in.Dev, msg)
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
