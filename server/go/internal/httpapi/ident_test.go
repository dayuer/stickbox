package httpapi

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/dayuer/stickbox/server/go/internal/profile"
)

const identBody = `{"serial":"CNB9K1P2X4","vid":"03F0","pid":"F22A",` +
	`"make":"HP","model":"HP Laser MFP 136a",` +
	`"cmd":"URF,PCL,PJL,PWGRaster","urf_caps":"CP1,RS600,V1.4,W8"}`

// 上报机型档案 → 建档 → 下发怪癖档案，一条龙。
func TestIdentBuildsPrinterAndPublishesProfile(t *testing.T) {
	h, dep := newTestAPI(t)
	appTok := login(t, h, dep, "13800008888")
	devKey := enroll(t, h, appTok, "f412fa87c9e0")

	if rr := post(h, "/api/device/f412fa87c9e0/ident", identBody, bearer(devKey)); rr.Code != 200 {
		t.Fatalf("上报 = %d %s", rr.Code, rr.Body)
	}

	p, ok, _ := dep.store.GetPrinter("CNB9K1P2X4")
	if !ok {
		t.Fatal("打印机没建档")
	}
	if p.Model != "HP Laser MFP 136a" || p.LastDev != "f412fa87c9e0" {
		t.Errorf("建档内容 = %+v", p)
	}

	body, ok := dep.pub.profiles["f412fa87c9e0"]
	if !ok {
		t.Fatal("没有下发怪癖档案")
	}
	prof, err := profile.Unmarshal(body)
	if err != nil {
		t.Fatalf("下发的档案不是合法 JSON：%v", err)
	}
	// 档案必须带序列号——设备靠它判断该不该套用（换了打印机就该忽略）
	if prof.Serial != "CNB9K1P2X4" {
		t.Errorf("档案里的 serial = %q", prof.Serial)
	}
	if prof.Src != profile.SrcDefault {
		t.Errorf("首次上报应拿到默认档案，得到 %q", prof.Src)
	}
	// 默认必须发 UEL——不发只能打第一份
	if len(prof.Hooks.JobEnd) != 1 || prof.Hooks.JobEnd[0].Data != profile.UEL {
		t.Errorf("默认档案没发 UEL：%+v", prof.Hooks.JobEnd)
	}
	// 上限是硬约束，设备可用堆只有几十 KB
	if len(body) > profile.MaxBytes {
		t.Errorf("下发了 %d 字节，超过 %d", len(body), profile.MaxBytes)
	}
}

// 没有序列号就建不了档——那是区分「换打印机」和「换桥」的唯一依据。
// 但 ident 本身仍要落盘并返回 200，否则设备会反复重试。
func TestIdentWithoutSerialStillSucceeds(t *testing.T) {
	h, dep := newTestAPI(t)
	appTok := login(t, h, dep, "13800008888")
	devKey := enroll(t, h, appTok, "f412fa87c9e0")

	rr := post(h, "/api/device/f412fa87c9e0/ident", `{"make":"HP","model":"X"}`, bearer(devKey))
	if rr.Code != 200 {
		t.Fatalf("= %d %s", rr.Code, rr.Body)
	}
	if len(dep.pub.profiles) != 0 {
		t.Error("没有序列号却下发了档案")
	}
}

// 老格式（字段在 printer_class 下面）也要认。
func TestIdentAcceptsNestedFormat(t *testing.T) {
	h, dep := newTestAPI(t)
	appTok := login(t, h, dep, "13800008888")
	devKey := enroll(t, h, appTok, "f412fa87c9e0")

	body := `{"vid":"03F0","pid":"F22A","printer_class":{"serial":"S1",` +
		`"make":"HP","model":"M","urf":"RS600,W8"}}`
	if rr := post(h, "/api/device/f412fa87c9e0/ident", body, bearer(devKey)); rr.Code != 200 {
		t.Fatalf("= %d %s", rr.Code, rr.Body)
	}
	p, ok, _ := dep.store.GetPrinter("S1")
	if !ok || p.URFCaps != "RS600,W8" {
		t.Errorf("嵌套格式没解析出来：%+v ok=%v", p, ok)
	}
}

// 换打印机：两台各自建档，档案跟着当前这台走。
func TestIdentTwoPrintersOnSameBridge(t *testing.T) {
	h, dep := newTestAPI(t)
	appTok := login(t, h, dep, "13800008888")
	devKey := enroll(t, h, appTok, "f412fa87c9e0")

	post(h, "/api/device/f412fa87c9e0/ident", identBody, bearer(devKey))
	second := `{"serial":"OTHER-1","vid":"04A9","pid":"1234","make":"Canon",` +
		`"model":"LBP","urf_caps":"RS600,W8"}`
	post(h, "/api/device/f412fa87c9e0/ident", second, bearer(devKey))

	list, err := dep.store.PrintersOfDevice("f412fa87c9e0")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("这个桥见过 %d 台，期望 2", len(list))
	}
	prof, _ := profile.Unmarshal(dep.pub.profiles["f412fa87c9e0"])
	if prof.Serial != "OTHER-1" {
		t.Errorf("最后下发的档案对应 %q，期望当前这台 OTHER-1", prof.Serial)
	}
}

func TestPrinterEndpointShowsProfileSource(t *testing.T) {
	h, dep := newTestAPI(t)
	appTok := login(t, h, dep, "13800008888")
	devKey := enroll(t, h, appTok, "f412fa87c9e0")
	post(h, "/api/device/f412fa87c9e0/ident", identBody, bearer(devKey))

	rr := get(h, "/api/device/f412fa87c9e0/printer", bearer(appTok))
	if rr.Code != 200 {
		t.Fatalf("= %d %s", rr.Code, rr.Body)
	}
	var out struct {
		Printer struct {
			Serial string `json:"serial"`
			Model  string `json:"model"`
		} `json:"printer"`
		Profile struct {
			Src string `json:"src"`
		} `json:"profile"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Printer.Serial != "CNB9K1P2X4" || out.Printer.Model != "HP Laser MFP 136a" {
		t.Errorf("printer = %+v", out.Printer)
	}
	// src 告诉用户这份配置的可信度：default 就该提示做一次测试
	if out.Profile.Src != "default" {
		t.Errorf("src = %q，期望 default", out.Profile.Src)
	}
}

func TestPrinterEndpoint404WithoutIdent(t *testing.T) {
	h, dep := newTestAPI(t)
	token := login(t, h, dep, "13800008888")
	enroll(t, h, token, "f412fa87c9e0")
	if rr := get(h, "/api/device/f412fa87c9e0/printer", bearer(token)); rr.Code != 404 {
		t.Errorf("= %d，期望 404", rr.Code)
	}
}

// 真机上报的第 0 层档案。样本直接取自 /opt/stickbox/idents/，不是编的。
//
// 这条测试是为一个线上故障写的：parseIdent 一度只找顶层 serial 和
// printer_class.serial，而真实结构把序列号放在 usb.serial。结果每次第 0 层
// 上报都判「没有序列号」跳过建档，打印机建不了档，App 里 make/model 一直
// 是空串——而日志只说「跳过建档」，看不出是字段路径错了。
func TestParseIdentRealHP136a(t *testing.T) {
	raw, err := os.ReadFile("testdata/ident-hp136a-base.json")
	if err != nil {
		t.Skipf("没有真机样本：%v", err)
	}
	p, ok := parseIdent(raw)
	if !ok {
		t.Fatal("真机样本解析不出打印机——序列号找不到就建不了档")
	}
	if p.Serial != "CNB1S5N3S7" {
		t.Errorf("serial = %q，期望 CNB1S5N3S7（它在 usb.serial 下）", p.Serial)
	}
	if p.VID != "03F0" || p.PID != "F22A" {
		t.Errorf("vid/pid = %q/%q，期望 03F0/F22A（usb 下是带 0x 前缀的写法）", p.VID, p.PID)
	}
	if p.Make == "" {
		t.Error("make 是空的")
	}
	// 型号取 IEEE-1284 设备 ID 里的 MDL——打印机的自述才是真相，
	// 网上的型号资料不是。
	if !strings.Contains(p.Model, "HP Laser MFP") {
		t.Errorf("model = %q，期望含 HP Laser MFP", p.Model)
	}
	// CMD 决定 App 能不能发 URF。取不到等于不知道这台机器认什么格式。
	if !strings.Contains(p.CMD, "URF") {
		t.Errorf("cmd = %q，期望含 URF", p.CMD)
	}
}

func TestDevIDField(t *testing.T) {
	const id = "MFG:HP;CMD:SPL,URF,FWV;MDL:HP Laser MFP 131 133 135-138;CLS:PRINTER;"
	for _, c := range []struct{ key, want string }{
		{"MFG", "HP"},
		{"CMD", "SPL,URF,FWV"},
		{"MDL", "HP Laser MFP 131 133 135-138"},
		{"mdl", "HP Laser MFP 131 133 135-138"}, // 大小写不敏感
		{"NOPE", ""},
	} {
		if got := devIDField(id, c.key); got != c.want {
			t.Errorf("devIDField(%q) = %q，期望 %q", c.key, got, c.want)
		}
	}
}
