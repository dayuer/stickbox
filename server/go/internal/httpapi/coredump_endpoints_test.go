package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uploadDump 发原始 ELF（非 JSON）请求体，带可选头部。
func uploadDump(t *testing.T, h http.Handler, path, body string, hdr map[string]string) int {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	rr := do(h, req, hdr)
	return rr.Code
}

func TestCoredumpUploadStoresElfAndMeta(t *testing.T) {
	h, dep := newTestAPI(t)
	tok, _ := register(t, h, "f412fa87c9e0")

	code := uploadDump(t, h, "/api/device/coredump", "\x7fELFcrash", map[string]string{
		"Authorization":      "Bearer " + tok,
		"X-Stickbox-Version": "1.2.0",
		"X-Stickbox-Reset":   "panic",
	})
	if code != 200 {
		t.Fatalf("上传 = %d", code)
	}

	dir := filepath.Join(dep.cfg.Root, "coredumps", "f412fa87c9e0")
	elfs, metas := listByExt(t, dir)
	if len(elfs) != 1 || len(metas) != 1 {
		t.Fatalf("目录里 elf=%v json=%v", elfs, metas)
	}
	raw, err := os.ReadFile(filepath.Join(dir, elfs[0]))
	if err != nil || string(raw) != "\x7fELFcrash" {
		t.Errorf("ELF 内容 = %q err=%v", raw, err)
	}
	var meta struct {
		Dev, FW, Reset string
		Size           int
	}
	mb, _ := os.ReadFile(filepath.Join(dir, metas[0]))
	if err := json.Unmarshal(mb, &meta); err != nil {
		t.Fatalf("元数据不是 JSON：%s", mb)
	}
	if meta.Dev != "f412fa87c9e0" || meta.FW != "1.2.0" || meta.Reset != "panic" || meta.Size != 9 {
		t.Errorf("元数据 = %+v", meta)
	}
	// 不该留下半成品。
	if parts, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(parts) != 0 {
		t.Errorf("残留临时文件 %v", parts)
	}
}

// 超过上限要拒掉，否则拿到一把设备密钥就能把磁盘撑爆。
func TestCoredumpTooLargeRejected(t *testing.T) {
	h, dep := newTestAPI(t)
	tok, _ := register(t, h, "f412fa87c9e0")

	big := strings.Repeat("x", maxCoredumpBytes+1)
	if code := uploadDump(t, h, "/api/device/coredump", big, bearer(tok)); code != 413 {
		t.Errorf("超大上传 = %d，期望 413", code)
	}
	dir := filepath.Join(dep.cfg.Root, "coredumps", "f412fa87c9e0")
	if elfs, _ := listByExt(t, dir); len(elfs) != 0 {
		t.Errorf("被拒的上传不该落盘，却有 %v", elfs)
	}
}

func TestCoredumpEmptyRejected(t *testing.T) {
	h, _ := newTestAPI(t)
	tok, _ := register(t, h, "f412fa87c9e0")
	if code := uploadDump(t, h, "/api/device/coredump", "", bearer(tok)); code != 400 {
		t.Errorf("空上传 = %d，期望 400", code)
	}
}

func TestCoredumpRequiresDeviceAuth(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")

	if c := uploadDump(t, h, "/api/device/coredump", "x", nil); c != 401 {
		t.Errorf("无令牌 = %d，期望 401", c)
	}
	if c := uploadDump(t, h, "/api/device/coredump", "x", bearer(agentTok)); c != 401 {
		t.Errorf("agent 令牌 = %d，期望 401", c)
	}
}

// 头部是设备传来的，不能原样落盘——密钥泄露后别人可以投任意值。
func TestCoredumpSanitizesHeaders(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1.2.0", "1.2.0"},
		{`a"b`, "ab"},
		{"../../etc/passwd", "....etcpasswd"},
		{"panic\nX-Evil: 1", "panicX-Evil1"},
		{strings.Repeat("v", 100), strings.Repeat("v", 64)},
	} {
		if got := sanitizeHeader(tc.in); got != tc.want {
			t.Errorf("sanitizeHeader(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// 超过保留份数时删最旧的，磁盘不能无限涨。
func TestCoredumpPrunesOldest(t *testing.T) {
	h, dep := newTestAPI(t)
	tok, _ := register(t, h, "f412fa87c9e0")
	dir := filepath.Join(dep.cfg.Root, "coredumps", "f412fa87c9e0")
	os.MkdirAll(dir, 0o755)

	// 先造一批旧的，文件名就是时间戳，排序即时间序。
	for i := 0; i < keepCoredumpsPerDev+5; i++ {
		name := fmt.Sprintf("20200101-0000%02d", i)
		os.WriteFile(filepath.Join(dir, name+".elf"), []byte("old"), 0o644)
		os.WriteFile(filepath.Join(dir, name+".json"), []byte("{}"), 0o644)
	}
	if code := uploadDump(t, h, "/api/device/coredump", "\x7fELFnew", bearer(tok)); code != 200 {
		t.Fatalf("上传 = %d", code)
	}

	elfs, metas := listByExt(t, dir)
	if len(elfs) != keepCoredumpsPerDev {
		t.Errorf("保留 %d 份，期望 %d", len(elfs), keepCoredumpsPerDev)
	}
	if len(metas) != len(elfs) {
		t.Errorf("elf %d 份但 json %d 份，元数据没跟着删", len(elfs), len(metas))
	}
	// 最旧的那几份应当没了，新传的那份还在。
	if elfs[0] == "20200101-000000.elf" {
		t.Error("最旧的一份没被删掉")
	}
}

// agent 只能看到自己名下设备的崩溃记录。
func TestAgentCoredumpsListsOwnOnly(t *testing.T) {
	h, dep := newTestAPI(t)
	devTok, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")
	_ = devTok

	// 配对会换密钥，用新密钥传一份。
	newTok, _ := register(t, h, "f412fa87c9e0")
	_ = newTok
	// 另一台不属于这个 agent 的设备，直接在磁盘上造一份记录。
	other := filepath.Join(dep.cfg.Root, "coredumps", "aabbccddeeff")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "20200101-000000.elf"), []byte("x"), 0o644)

	rr := get(h, "/api/agent/coredumps", bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("= %d %s", rr.Code, rr.Body)
	}
	var out []struct {
		Dev string `json:"dev"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	for _, e := range out {
		if e.Dev == "aabbccddeeff" {
			t.Error("列出了别人的设备")
		}
	}
}

func listByExt(t *testing.T, dir string) (elfs, metas []string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	for _, e := range ents {
		switch {
		case strings.HasSuffix(e.Name(), ".elf"):
			elfs = append(elfs, e.Name())
		case strings.HasSuffix(e.Name(), ".json"):
			metas = append(metas, e.Name())
		}
	}
	return elfs, metas
}
