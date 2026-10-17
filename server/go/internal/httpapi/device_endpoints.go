package httpapi

import (
	"net/http"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

// handleEnroll 为一台设备签发 device 密钥。
//
// 「重置」不另设机制：给一个已属于本用户的 dev 重新 enroll，就是重置。
// 用户按住按键恢复出厂时设备清了 NVS，服务端全程不知情——落在 enroll 上
// 才不需要任何额外协调。
func (a *API) handleEnroll(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	var in struct {
		Dev  string `json:"dev"`
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	// dev 可选。
	//
	// 不带 dev 时签发一把**待认领**的密钥：App 在还没连上设备热点的时候就能
	// 拿到它，连一次热点写进去就完事。设备首次连 MQTT 时用自己的 dev 做
	// username，服务端从那里学到 MAC 并完成绑定——服务端本来就能知道，
	// 让 App 先去问设备要是多余的，而那一步正是「配网要连两次热点」的根源。
	//
	// 带 dev 时是老路径：立刻绑定，抢绑冲突当场就能报 409。
	if in.Dev != "" && !auth.ValidDev(in.Dev) {
		fail(w, 400, "bad dev", "dev 必须是 12 位小写十六进制的 MAC")
		return
	}

	owner, bound, err := a.store.OwnerOfDevice(in.Dev)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if in.Dev == "" {
		bound = false // 待认领的密钥没有「重置」一说
	}
	// 抢绑防护：服务端无法验证 App 是否真的物理接触了设备，
	// 不设防的话任何人拿别人的 MAC 就能重置别人的设备。
	if bound && owner != id.UserID {
		fail(w, 409, "device bound", "该设备已绑定到其他账号，需原持有人先解绑")
		return
	}
	if bound {
		if err := auth.RevokeDeviceKeys(a.store, a.v, in.Dev); err != nil {
			fail(w, 500, "server error", "")
			return
		}
	}
	token, err := auth.IssueDeviceKey(a.store, id.UserID, in.Dev, in.Name)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	a.mem.Invalidate(id.UserID) // 否则新绑的设备最多要等 5 分钟才能订阅成功
	writeJSON(w, 200, map[string]any{
		"device_key": token, "dev": in.Dev, "reset": bound,
	})
}

func (a *API) handleUnbind(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	dev := r.PathValue("dev")
	owner, bound, err := a.store.OwnerOfDevice(dev)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !bound {
		writeJSON(w, 200, map[string]int{"ok": 1}) // 幂等
		return
	}
	if owner != id.UserID {
		fail(w, 403, "forbidden", "")
		return
	}
	if err := auth.RevokeDeviceKeys(a.store, a.v, dev); err != nil {
		fail(w, 500, "server error", "")
		return
	}
	a.mem.Invalidate(id.UserID)
	writeJSON(w, 200, map[string]int{"ok": 1})
}

// handleStatus 返回设备是否在线。在线判据是最近 120 秒内收到过 MQTT 心跳。
//
// App 用 X-Device 查自己名下的设备；设备角色只能查自己。
func (a *API) handleStatus(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	dev := id.Dev
	if id.Role == auth.RoleApp {
		dev = r.Header.Get("X-Device")
		if !a.ownDevice(id, dev) {
			fail(w, 403, "forbidden", "")
			return
		}
	}
	writeJSON(w, 200, map[string]any{
		"device": map[string]any{
			"dev":    dev,
			"online": a.reg.IsOnline(dev),
		},
	})
}
