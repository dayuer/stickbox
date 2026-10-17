package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dayuer/stickbox/server/go/internal/config"
)

func ptr(n int) *int { return &n }

// setFirmware 改 API 持有的那份配置（New 存的是指针，构造后改仍然生效）。
func setFirmware(dep *testDeps, fw config.Firmware) { dep.cfg.Firmware = fw }

// —— manifest ——

// 没配固件就回 204。设备见到非 200 安静跳过，不当错误。
func TestFirmwareNotConfiguredReturns204(t *testing.T) {
	h, dep := newTestAPI(t)
	tok, _ := register(t, h, "f412fa87c9e0")
	_ = dep

	rr := get(h, "/api/device/firmware", bearer(tok))
	if rr.Code != 204 {
		t.Errorf("没配固件 = %d，期望 204（body=%s）", rr.Code, rr.Body)
	}
}

// URL 空也算没配——只填版本号不填下载地址是半套配置，不能下发。
func TestFirmwareVersionWithoutURLReturns204(t *testing.T) {
	h, dep := newTestAPI(t)
	tok, _ := register(t, h, "f412fa87c9e0")
	setFirmware(dep, config.Firmware{Version: "1.2.0"})

	if rr := get(h, "/api/device/firmware", bearer(tok)); rr.Code != 204 {
		t.Errorf("只有版本号没有 URL = %d，期望 204", rr.Code)
	}
}

func TestFirmwareManifest(t *testing.T) {
	h, dep := newTestAPI(t)
	tok, _ := register(t, h, "f412fa87c9e0")
	setFirmware(dep, config.Firmware{
		Version: "1.2.0",
		URL:     "https://mqtt.stickbox.ai/fw/stickbox-1.2.0.bin",
		SHA256:  "abc123",
		Size:    2063040,
	})

	rr := get(h, "/api/device/firmware", bearer(tok))
	if rr.Code != 200 {
		t.Fatalf("= %d %s", rr.Code, rr.Body)
	}
	var out struct {
		Version string `json:"version"`
		URL     string `json:"url"`
		SHA256  string `json:"sha256"`
		Size    int64  `json:"size"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON：%s", rr.Body)
	}
	if out.Version != "1.2.0" || out.SHA256 != "abc123" || out.Size != 2063040 ||
		out.URL != "https://mqtt.stickbox.ai/fw/stickbox-1.2.0.bin" {
		t.Errorf("manifest = %+v", out)
	}
	// 设备的 esp_http_client 不解 chunked，必须有 Content-Length。
	if rr.Header().Get("Content-Length") == "" {
		t.Error("缺 Content-Length，设备读不了响应体")
	}
}

// 没令牌、agent 令牌都不能读设备的升级通道。
func TestFirmwareRequiresDeviceAuth(t *testing.T) {
	h, dep := newTestAPI(t)
	devTok, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")
	setFirmware(dep, config.Firmware{Version: "1.2.0", URL: "https://x/f.bin"})

	if rr := get(h, "/api/device/firmware", nil); rr.Code != 401 {
		t.Errorf("无令牌 = %d，期望 401", rr.Code)
	}
	if rr := get(h, "/api/device/firmware", bearer(agentTok)); rr.Code != 401 {
		t.Errorf("agent 令牌 = %d，期望 401", rr.Code)
	}
	// 配对会换掉待配对密钥，所以这里重新注册一台来确认设备角色能读。
	_ = devTok
	tok2, _ := register(t, h, "aabbccddeeff")
	if rr := get(h, "/api/device/firmware", bearer(tok2)); rr.Code != 200 {
		t.Errorf("设备令牌 = %d，期望 200", rr.Code)
	}
}

// —— 分阶段放量 ——

func TestRolloutZeroShipsToNobody(t *testing.T) {
	h, dep := newTestAPI(t)
	tok, _ := register(t, h, "f412fa87c9e0")
	setFirmware(dep, config.Firmware{
		Version: "1.2.0", URL: "https://x/f.bin", RolloutPercent: ptr(0),
	})
	if rr := get(h, "/api/device/firmware", bearer(tok)); rr.Code != 204 {
		t.Errorf("rollout=0 = %d，期望 204", rr.Code)
	}
}

// 字段缺省按 100 处理——「没写」和「写了 0」后果相反，不能混为一谈。
func TestRolloutAbsentDefaultsTo100(t *testing.T) {
	var fw config.Firmware
	if got := fw.RolloutPercentOrDefault(); got != 100 {
		t.Errorf("缺省 = %d，期望 100", got)
	}
	fw.RolloutPercent = ptr(0)
	if got := fw.RolloutPercentOrDefault(); got != 0 {
		t.Errorf("显式 0 = %d，期望 0", got)
	}
}

// 按 dev 哈希分桶，不是随机：同一台设备反复问得到同一个答案，
// 否则设备会今天在、明天不在地反复横跳。
func TestRolloutIsStablePerDevice(t *testing.T) {
	const dev = "f412fa87c9e0"
	want := inRollout(dev, 50)
	for i := 0; i < 100; i++ {
		if inRollout(dev, 50) != want {
			t.Fatalf("同一 dev 第 %d 次判定翻转了", i)
		}
	}
}

// 放量比例调大时，原先已经在里面的设备必须仍然在——否则升到一半的设备
// 会被踢出，陷入「下次检查又说没有新版本」。
func TestRolloutIsMonotonic(t *testing.T) {
	devs := []string{"f412fa87c9e0", "aabbccddeeff", "001122334455", "ffeeddccbbaa",
		"0123456789ab", "cafebabe0001", "deadbeef0002", "8badf00d0003"}
	for _, d := range devs {
		in := false
		for p := 0; p <= 100; p++ {
			now := inRollout(d, p)
			if in && !now {
				t.Errorf("dev=%s 在 %d%% 时被踢出放量", d, p)
			}
			in = now
		}
		if !in {
			t.Errorf("dev=%s 到 100%% 仍不在放量里", d)
		}
	}
}

// 分桶大致均匀即可，不要求精确——只要不是全进或全不进。
func TestRolloutRoughlySplits(t *testing.T) {
	n, in := 0, 0
	for a := 0; a < 40; a++ {
		for b := 0; b < 10; b++ {
			dev := string(rune('a'+a%26)) + "412fa87c9e" + string(rune('0'+b))
			n++
			if inRollout(dev, 50) {
				in++
			}
		}
	}
	if in < n/5 || in > n*4/5 {
		t.Errorf("50%% 放量命中 %d/%d，分桶明显不均", in, n)
	}
}

// —— 固件下载 ——

func TestFirmwareFileServesAndRejectsTraversal(t *testing.T) {
	h, dep := newTestAPI(t)
	dir := filepath.Join(dep.cfg.Root, "fw")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("\xe9\x00\x00\x00firmware")
	if err := os.WriteFile(filepath.Join(dir, "stickbox-1.2.0.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	// 放一个本不该被取到的文件在 Root 下（fw 的上一层）。
	if err := os.WriteFile(filepath.Join(dep.cfg.Root, "config.json"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 固件 bin 不鉴权：开源硬件，二进制不是秘密，防篡改靠签名。
	rr := get(h, "/fw/stickbox-1.2.0.bin", nil)
	if rr.Code != 200 {
		t.Fatalf("下载 = %d %s", rr.Code, rr.Body)
	}
	if rr.Body.String() != string(body) {
		t.Error("下载内容不一致")
	}
	if rr.Header().Get("Content-Length") == "" {
		t.Error("缺 Content-Length，esp_https_ota 读不了")
	}

	// 第一道：ServeMux 的 {name} 只匹配单个路径段，加上它自己的路径清理，
	// 这些请求根本到不了 handler。
	for _, p := range []string{
		"/fw/..%2fconfig.json",
		"/fw/%2e%2e%2fconfig.json",
		"/fw/../config.json",
		"/fw/不存在.bin",
	} {
		rr := get(h, p, nil)
		if rr.Code == 200 {
			t.Errorf("%s = 200 body=%q，期望非 200", p, rr.Body)
		}
	}
}

// 第二道：直接把带路径的 {name} 喂给 handler，绕过 ServeMux 的保护，
// 确认 filepath.Base 自己也能挡住。改用通配模式时这条会先炸。
func TestFirmwareFileSanitizesNameItself(t *testing.T) {
	h, dep := newTestAPI(t)
	_ = h
	if err := os.WriteFile(filepath.Join(dep.cfg.Root, "config.json"),
		[]byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := New(dep.cfg, dep.store, dep.v, dep.reg, dep.pub)

	for _, name := range []string{"../config.json", "../../etc/passwd", "..", "."} {
		req := httptest.NewRequest("GET", "/fw/x", nil)
		req.SetPathValue("name", name)
		rr := httptest.NewRecorder()
		api.handleFirmwareFile(rr, req)
		if rr.Code == 200 {
			t.Errorf("name=%q = 200 body=%q，应当被 Base() 挡住", name, rr.Body)
		}
	}
}
