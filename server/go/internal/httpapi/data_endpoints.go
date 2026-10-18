package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/sales"
)

// 经营数据这一路的上报端点叫 /api/data 而不是 /api/sales：它是 agent 面向的
// 「非 Claude 那一路数据」通道，指标集合以后会变（现在是成交额/订单/权益卡
// 收入），路径不该跟着指标名走。落库与换算在 internal/sales。

// handleDataPost：agent 上报一次当日经营数据，落库后标脏让帧重渲。
func (a *API) handleDataPost(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	var rep sales.Report
	// 几个数字加一个日期，4KB 够了。
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&rep); err != nil {
		fail(w, 400, "bad request", "")
		return
	}
	if err := a.sales.Ingest(id.UserID, rep); errors.Is(err, sales.ErrInvalid) {
		fail(w, 400, "bad request", err.Error())
		return
	} else if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	a.store.TouchKey(id.KeyID) // status 里显示「这台电脑最后一次上报」
	// 数据变了只标脏；后台 worker 防抖约 2 秒后重渲染。
	a.markFramesDirty(id.UserID)
	writeJSON(w, 200, map[string]int{"ok": 1})
}

// handleDataGet：GET /api/agent/data —— agent 查看自己用户的最新经营数据。
func (a *API) handleDataGet(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	rep, received, ok, err := a.sales.Latest(id.UserID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !ok {
		fail(w, 404, "not found", "还没有上报过")
		return
	}
	writeJSON(w, 200, map[string]any{
		"ts": received.Unix(), "date": rep.Date, "tz": rep.TZ,
		"gmv": rep.GMV, "orders": rep.Orders, "card_revenue": rep.CardRevenue,
	})
}
