package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/profile"
	"github.com/dayuer/stickbox/server/go/internal/raster"
)

// handleEnroll 为一台桥签发 device 密钥。
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
	// 带 dev 时是老路径：立刻绑定，抢绑冲突当场就能报 409。App 如果已经
	// 读到过 dev（比如连上热点之后重配），带上它能更早发现冲突。
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

// handleRenderProfile 下发光栅参数。App 在光栅之前必须先拉这个，不要硬编码——
// 换打印机时改的是服务端，App 不用发版。
func (a *API) handleRenderProfile(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	dev := r.PathValue("dev")
	if !a.ownDevice(id, dev) {
		fail(w, 403, "forbidden", "")
		return
	}
	caps, serial, ok := a.loadIdentCaps(dev)
	if !ok {
		fail(w, 404, "no printer", "该设备尚未上报机型档案——没插打印机或还没枚举完")
		return
	}
	p, err := raster.ParseCaps(caps)
	if err != nil {
		fail(w, 500, "bad caps", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"dev": dev, "serial": serial, "format": p.Format,
		"urf_caps": p.Caps, "dpi": p.DPI, "color": p.Color, "pages": p.Pages,
		// 实测值，不是从能力串推的：不可打印区必须出纸才测得出（HANDOFF 3.6 第 2 层）
		"margins_mm":    []int{4, 4, 4, 4},
		"max_job_bytes": maxJobBytes,
	})
}

// handlePrinters 列出这个桥见过的所有打印机及各自的排队数。
// 用户看不到排队数就会以为打印失败了。
func (a *API) handlePrinters(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	dev := r.PathValue("dev")
	if !a.ownDevice(id, dev) {
		fail(w, 403, "forbidden", "")
		return
	}
	attached := ""
	if act := a.reg.Actor(dev); act != nil {
		attached = act.Serial()
	}
	counts, err := a.store.QueuedCountByPrinter(dev)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	type row struct {
		Serial     string `json:"serial"`
		Attached   bool   `json:"attached"`
		QueuedJobs int    `json:"queued_jobs"`
	}
	rows := []row{}
	seen := map[string]bool{}
	for ser, n := range counts {
		if ser == "" {
			continue
		}
		rows = append(rows, row{ser, ser == attached, n})
		seen[ser] = true
	}
	if attached != "" && !seen[attached] {
		rows = append(rows, row{attached, true, 0})
	}
	writeJSON(w, 200, map[string]any{"attached": attached, "printers": rows})
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	dev := id.Dev
	if id.Role == auth.RoleApp {
		dev = r.Header.Get("X-Device")
		if !a.ownDevice(id, dev) {
			fail(w, 403, "forbidden", "")
			return
		}
	}
	jobs, err := a.store.JobsForDevice(dev, 15)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	out := map[string]any{"jobs": jobs}
	if act := a.reg.Actor(dev); act != nil {
		d := map[string]any{"dev": dev, "online": true, "serial": act.Serial()}
		if p := act.Printer(); len(p) > 0 {
			d["prn"] = json.RawMessage(p)
		} else {
			d["prn"] = nil
		}
		// USB 事件计数（文档 3.6）。摊在这里是为了随时能查——
		// 「插回去认不出来」那类故障只在设备上表现为一片安静。
		if u := act.USB(); len(u) > 0 {
			d["usb"] = json.RawMessage(u)
		}
		// 空闲堆。设备只有几十 KB 可用，一条 TLS 连接的缓冲就占十几 KB，
		// 堆一紧握手就以「证书错」的形态失败——不摊在这里，现场只能靠
		// 串口才能看出真因，而用户现场的板子串口够不着。
		if hp := act.Heap(); hp > 0 {
			d["heap"] = hp
		}
		out["device"] = d
	} else {
		out["device"] = map[string]any{"dev": dev, "online": false, "prn": nil}
	}
	writeJSON(w, 200, out)
}

// loadIdentCaps 从最近一份 ident 里取出 URF 能力串和打印机序列号。
func (a *API) loadIdentCaps(dev string) (caps, serial string, ok bool) {
	raw, err := os.ReadFile(filepath.Join(a.cfg.IdentsDir(), dev, "latest.json"))
	if err != nil {
		return "", "", false
	}
	var doc struct {
		URFCaps string `json:"urf_caps"`
		Serial  string `json:"serial"`
		Printer struct {
			URFCaps string `json:"urf"`
			Serial  string `json:"serial"`
		} `json:"printer_class"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", "", false
	}
	caps, serial = doc.URFCaps, doc.Serial
	if caps == "" {
		caps = doc.Printer.URFCaps
	}
	if serial == "" {
		serial = doc.Printer.Serial
	}
	return caps, serial, caps != ""
}

// handlePrinter 返回当前插着的打印机的完整信息与生效档案。
// App 用它告诉用户「这台打印机是什么、当前配置可信到什么程度」。
func (a *API) handlePrinter(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	dev := r.PathValue("dev")
	if !a.ownDevice(id, dev) {
		fail(w, 403, "forbidden", "")
		return
	}
	serial := ""
	if act := a.reg.Actor(dev); act != nil {
		serial = act.Serial()
	}
	if serial == "" {
		// 设备不在线或没插打印机时，退回它最近插过的那台
		list, err := a.store.PrintersOfDevice(dev)
		if err != nil {
			fail(w, 500, "server error", "")
			return
		}
		if len(list) == 0 {
			fail(w, 404, "no printer", "该设备尚未上报过机型档案")
			return
		}
		serial = list[0].Serial
	}
	p, ok, err := a.store.GetPrinter(serial)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !ok {
		fail(w, 404, "no printer", "")
		return
	}
	prof := profile.Lookup(a.store, p.Serial, p.VID, p.PID, p.Model)
	writeJSON(w, 200, map[string]any{"printer": p, "profile": prof})
}
