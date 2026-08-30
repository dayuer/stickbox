package httpapi

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/dayuer/stickbox/server/go/internal/profile"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

// identDoc 是设备上报的机型档案里我们关心的那部分。
//
// 全量档案有 4~16KB（第 0 层描述符 + 第 1 层 PJL 探针），这里只挑出
// 建档和下发 profile 需要的字段，其余原样落盘。
//
// **真实结构是三段：{"usb":{...},"printer_class":{...},"profile":{...}}。**
// 序列号在 usb.serial，型号在 printer_class.model，能力串在 device_id 里。
// 这里一度只找顶层 serial 和 printer_class.serial，两个都不在，结果每次
// 第 0 层上报都判「没有序列号」跳过建档——打印机建不了档，App 里
// make/model 一直是空串，而日志只说「跳过建档」，看不出是字段路径错了。
//
// 三层都读，是因为固件的 dump 格式改过几版，老设备还在网上。
type identDoc struct {
	// 顶层（文档 4.3 的写法）
	Serial  string `json:"serial"`
	VID     string `json:"vid"`
	PID     string `json:"pid"`
	Make    string `json:"make"`
	Model   string `json:"model"`
	CMD     string `json:"cmd"`
	URFCaps string `json:"urf_caps"`

	// 当前固件：第 0 层描述符全在 usb 下面
	USB struct {
		VID          string `json:"vid"`
		PID          string `json:"pid"`
		Serial       string `json:"serial"`
		Manufacturer string `json:"manufacturer"`
		Product      string `json:"product"`
	} `json:"usb"`

	// 打印机类接口：IEEE-1284 设备 ID 和从中解出的型号
	PrinterClass struct {
		Serial   string `json:"serial"`
		Make     string `json:"make"`
		Model    string `json:"model"`
		CMD      string `json:"cmd"`
		URFCaps  string `json:"urf"`
		DeviceID string `json:"device_id"`
	} `json:"printer_class"`
}

// devIDField 从 IEEE-1284 设备 ID 串里取一个字段。
//
// 形如 `MFG:HP;CMD:SPL,URF,...;MDL:HP Laser MFP 131 133 135-138;`。
// 打印机的自述能力表才是真相，网上的型号资料不是——所以 CMD 只能从这里取。
func devIDField(devID, key string) string {
	for _, part := range strings.Split(devID, ";") {
		k, v, ok := strings.Cut(part, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func parseIdent(raw []byte) (store.Printer, bool) {
	var d identDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return store.Printer{}, false
	}
	pick := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	devID := d.PrinterClass.DeviceID
	// VID/PID 在 usb 下是 "0x03F0" 这种带前缀的写法，库里存的是 "03F0"。
	trimHex := func(s string) string { return strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X") }

	p := store.Printer{
		Serial: pick(d.Serial, pick(d.USB.Serial, d.PrinterClass.Serial)),
		VID:    pick(d.VID, trimHex(d.USB.VID)),
		PID:    pick(d.PID, trimHex(d.USB.PID)),
		Make: pick(d.Make, pick(d.PrinterClass.Make,
			pick(d.USB.Manufacturer, devIDField(devID, "MFG")))),
		Model: pick(d.Model, pick(d.PrinterClass.Model,
			pick(devIDField(devID, "MDL"), d.USB.Product))),
		CMD:     pick(d.CMD, pick(d.PrinterClass.CMD, devIDField(devID, "CMD"))),
		URFCaps: pick(d.URFCaps, d.PrinterClass.URFCaps),
	}
	// 序列号是主键。没有它就建不了档——也区分不了
	// 「同一个桥换了打印机」和「同一台打印机换了桥」。
	return p, p.Serial != ""
}

// onIdent 在设备上报机型档案后建档并下发怪癖档案。
//
// 失败不影响 ident 上报本身返回 200：档案已经落盘了，下发失败下次重连
// 还会重来（profile 是 retain 消息）。让上报失败反而会让设备反复重试。
func (a *API) onIdent(dev string, raw []byte) {
	p, ok := parseIdent(raw)
	if !ok {
		slog.Warn("ident 里没有打印机序列号，跳过建档", "dev", dev)
		return
	}
	p.LastDev = dev
	if err := a.store.UpsertPrinter(p); err != nil {
		slog.Error("打印机建档失败", "dev", dev, "serial", p.Serial, "err", err)
		return
	}

	prof := profile.Lookup(a.store, p.Serial, p.VID, p.PID, p.Model)
	// 下发前把关：不能让畸形档案传到设备上才发现。
	if err := profile.Validate(prof); err != nil {
		slog.Error("查到的档案不合法，改发默认档案", "serial", p.Serial, "err", err)
		prof = profile.Default(p.Serial)
	}
	body, err := profile.Marshal(prof)
	if err != nil {
		slog.Error("档案编码失败", "serial", p.Serial, "err", err)
		return
	}
	if a.pub == nil {
		return
	}
	if err := a.pub.PublishProfile(dev, body); err != nil {
		slog.Error("档案下发失败", "dev", dev, "err", err)
		return
	}
	slog.Info("已下发怪癖档案", "dev", dev, "serial", p.Serial,
		"src", prof.Src, "bytes", len(body))
}
