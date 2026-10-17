package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

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
	a.store.TouchKey(id.KeyID) // status 里显示「这台电脑最后一次上报」
	// 用量变了就重渲染；失败只记日志，上报本身仍算成功。
	a.rebuildFrames(id.UserID)
	writeJSON(w, 200, map[string]any{"ok": 1, "devices": n})
}

// handleUsageGet：GET /api/agent/usage —— agent 查看自己用户的最新用量。
func (a *API) handleUsageGet(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	rep, received, ok, err := a.usage.Latest(id.UserID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !ok {
		fail(w, 404, "not found", "还没有上报过")
		return
	}
	writeJSON(w, 200, map[string]any{
		"ts": received.Unix(), "five_hour": rep.FiveHour, "seven_day": rep.SevenDay,
		"limits": rep.Limits,
	})
}

// handleAgentLogout：agent 自己注销。
func (a *API) handleAgentLogout(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	if err := auth.RevokeSession(a.store, a.v, id.KeyID); err != nil {
		fail(w, 500, "server error", "")
		return
	}
	writeJSON(w, 200, map[string]int{"ok": 1})
}
