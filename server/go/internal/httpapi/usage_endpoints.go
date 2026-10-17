package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

// handleAgentLogin：stickbox-agent 用手机号 + 验证码换一把 agent 令牌（API 文档 4.11）。
//
// 发码复用 POST /api/auth/sms。这里和 /api/auth/verify 的区别：
//   - 签发的是 agent 角色，只能上报用量；
//   - 不新建用户——没有账号、没绑设备的人上报用量也推不到任何屏幕上，
//     不如直接告诉他先去 App 里绑设备。
func (a *API) handleAgentLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
		Name  string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	phone, err := auth.NormalizePhone(in.Phone)
	if err != nil {
		fail(w, 400, "bad phone", "")
		return
	}
	hmac := a.phone.HMAC(phone)
	if err := a.sms.Verify(hmac, in.Code); err != nil {
		fail(w, 401, "bad code", "")
		return
	}
	u, found, err := a.store.UserByHMAC(hmac)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !found || u.Disabled {
		fail(w, 403, "no account", "先用 App 登录并绑定设备")
		return
	}
	name := in.Name
	if name == "" {
		name = "unknown"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	token, err := auth.IssueAgentKey(a.store, u.ID, name)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	writeJSON(w, 200, map[string]any{
		"token": token, "user_id": u.ID, "phone_tail": u.PhoneTail,
	})
}

// handleUsagePost：agent 上报一次用量，服务端落库并推给名下全部设备。
func (a *API) handleUsagePost(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	var rep usage.Report
	// limits 原样带上来，比别的端点的 4KB 上限宽一些
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&rep); err != nil {
		fail(w, 400, "bad request", "")
		return
	}
	n, err := a.usage.Ingest(id.UserID, rep)
	if errors.Is(err, usage.ErrInvalid) {
		fail(w, 400, "bad request", err.Error())
		return
	}
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	a.store.TouchKey(id.KeyID) // App 里显示「这台电脑最后一次上报」
	writeJSON(w, 200, map[string]any{"ok": 1, "devices": n})
}

// handleUsageGet：App 查看自己账号的最新用量。
func (a *API) handleUsageGet(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	rep, received, ok, err := a.usage.Latest(id.UserID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !ok {
		fail(w, 404, "not found", "还没有 agent 上报过")
		return
	}
	writeJSON(w, 200, map[string]any{
		"ts": received.Unix(), "five_hour": rep.FiveHour, "seven_day": rep.SevenDay,
		"limits": rep.Limits,
	})
}

// handleAgents：列出该账号下登录过的电脑。
func (a *API) handleAgents(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	keys, err := a.store.KeysOfUser(id.UserID, string(auth.RoleAgent))
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{
			"key_id": k.KeyID, "name": k.Name, "created": k.Created, "last_seen": k.LastSeen,
		})
	}
	writeJSON(w, 200, map[string]any{"agents": out})
}

// handleAgentRevoke：App 踢掉某台电脑。只能踢自己名下的 agent 令牌。
func (a *API) handleAgentRevoke(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	keyID := r.PathValue("key_id")
	k, found, err := a.store.KeyByID(keyID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	// 不是自己的、不是 agent 的，一律当不存在——不告诉对方这把钥匙存在
	if !found || k.UserID != id.UserID || k.Role != string(auth.RoleAgent) {
		fail(w, 404, "not found", "")
		return
	}
	if err := auth.RevokeSession(a.store, a.v, keyID); err != nil {
		fail(w, 500, "server error", "")
		return
	}
	writeJSON(w, 200, map[string]int{"ok": 1})
}

// handleAgentLogout：agent 自己注销（stickbox-agent logout）。
func (a *API) handleAgentLogout(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	if err := auth.RevokeSession(a.store, a.v, id.KeyID); err != nil {
		fail(w, 500, "server error", "")
		return
	}
	writeJSON(w, 200, map[string]int{"ok": 1})
}
