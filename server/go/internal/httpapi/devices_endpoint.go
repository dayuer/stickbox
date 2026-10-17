package httpapi

import (
	"net/http"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

// handleDevices 实现 GET /api/devices。
//
// 为什么需要这个端点：其余设备端点都要求调用方**已经知道** {dev}，而
// dev 唯一的来源是配网时从 SoftAP 读到的 MAC。没有它，App 的设备列表只能
// 存在手机本地——用户换手机或重装 App 之后，账号还在、设备还绑着，但 App
// 再也找不到它，只能把每台设备重新配一遍网。
//
// 不带 X-Device：这个端点就是用来得知有哪些 dev 的。
func (a *API) handleDevices(w http.ResponseWriter, r *http.Request, id auth.Identity) {
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

	// 一台设备都没有时返回 {"devices":[]}，不是 404——「没有设备」是正常状态，
	// 不是错误。App 靠这个区分「新用户」和「出问题了」。
	out := []row{}
	for _, dev := range devs {
		it := row{Dev: dev}
		if meta, ok, err := a.store.DeviceMeta(dev); err == nil && ok {
			it.Name = meta.Name
			it.Bound = meta.Bound
		}
		// 在线与否以最近一次 MQTT 心跳为准，跟 /api/status 用同一个判据。
		it.Online = a.reg.IsOnline(dev)
		out = append(out, it)
	}

	writeJSON(w, 200, map[string]any{"devices": out})
}
