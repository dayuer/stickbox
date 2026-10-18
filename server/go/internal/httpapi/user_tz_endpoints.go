package httpapi

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/tz"
)

// effectiveTZ 按「用户设置 > agent 上报 > default_tz」算出当前生效时区与来源。
func (a *API) effectiveTZ(userID string) (value, source string, err error) {
	userTZ, err := a.store.UserTZ(userID)
	if err != nil {
		return "", "", err
	}
	agentTZ := ""
	if rep, _, ok, err := a.usage.Latest(userID); err != nil {
		return "", "", err
	} else if ok {
		agentTZ = rep.TZ
	}
	value, source = tz.Resolve(userTZ, agentTZ, a.cfg.DefaultTZ)
	return value, source, nil
}

// handleUserTZGet：GET /api/user/tz
//
// 返回当前生效值和来源（user / agent / default）。时区优先级只在服务端
// 解析，agent 不需要（也不应该）自己算。
func (a *API) handleUserTZGet(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	value, source, err := a.effectiveTZ(id.UserID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	writeJSON(w, 200, map[string]string{"tz": value, "source": source})
}

// handleUserTZPut：PUT /api/user/tz {"tz":"Asia/Jakarta"}
//
// tz 为空串表示清除用户设置，回退到 agent 上报 / 服务端默认。
// 设置成功后同步重渲染该用户的帧并通知设备换帧，不等下一次用量上报。
func (a *API) handleUserTZPut(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	var in struct {
		TZ *string `json:"tz"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.TZ == nil {
		fail(w, 400, "bad request", `缺少 tz 字段（清除请传 ""）`)
		return
	}

	value := ""
	if strings.TrimSpace(*in.TZ) != "" {
		var err error
		value, err = tz.Normalize(*in.TZ)
		if err != nil {
			fail(w, 400, "bad tz", err.Error())
			return
		}
	}
	found, err := a.store.SetUserTZ(id.UserID, value)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !found {
		fail(w, 404, "not found", "用户不存在")
		return
	}

	// 立刻重渲。Rebuild 内部会递增版本并给名下设备发 frame 通知；
	// 渲染失败时退回后台重试，不让设置本身失败。
	if a.frames != nil {
		if err := a.frames.Rebuild(id.UserID); err != nil {
			slog.Warn("改时区后重渲失败，转后台重试",
				"user", id.UserID, "err", err)
			a.frames.MarkDirty(id.UserID)
		}
	}

	eff, source, err := a.effectiveTZ(id.UserID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	writeJSON(w, 200, map[string]string{"tz": eff, "source": source})
}
