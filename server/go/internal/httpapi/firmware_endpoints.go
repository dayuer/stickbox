package httpapi

import (
	"hash/fnv"
	"net/http"
	"os"
	"path/filepath"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

// 固件升级通道（T9）。
//
// 设备每天问一次「有没有新版本」。这里只回答，不做鉴权之外的判断——
// 要不要升级由设备比对版本号决定。
//
// 固件二进制本身不是秘密（项目是开源硬件），下载不鉴权；防篡改靠固件签名，
// 不靠访问控制。见 docs/PRODUCT.md 第 3 节。

// GET /api/device/firmware —— 设备鉴权。
//
// 有新版本返回 200 + manifest；没有配置固件、或这台设备不在本轮放量范围里，
// 返回 204。204 是「现在没有你的份」，不是错误——设备见到非 200 就安静跳过。
func (a *API) handleDeviceFirmware(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	fw := a.cfg.Firmware
	if fw.Version == "" || fw.URL == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !inRollout(id.Dev, fw.RolloutPercentOrDefault()) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version": fw.Version,
		"url":     fw.URL,
		"sha256":  fw.SHA256,
		"size":    fw.Size,
	})
}

// inRollout 决定这台设备是否在本轮放量里。
//
// 按 dev 哈希分桶，不是随机：同一台设备每次问得到的答案一致，不会今天在、
// 明天不在地反复横跳。放量比例调大时原先在的设备仍然在。
//
// 为什么要分阶段：一版坏固件如果一次推给所有设备，等发现时已经全军覆没。
// 先推 5%，看崩溃回传（T10）没异常再加到 100%。
func inRollout(dev string, percent int) bool {
	if percent >= 100 {
		return true
	}
	if percent <= 0 {
		return false
	}
	h := fnv.New32a()
	h.Write([]byte(dev))
	return int(h.Sum32()%100) < percent
}

// GET /fw/{name} —— 不鉴权的固件下载。
//
// 自托管的人不一定在前面架了 nginx，所以这里自带一个最小的静态服务。
// 只认 {Root}/fw 下的文件名，Base() 掉路径部分防目录穿越。
func (a *API) handleFirmwareFile(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name"))
	if name == "." || name == ".." || name == "/" {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(a.cfg.Root, "fw", name)
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	// ServeContent 带 Content-Length 和 Range，esp_https_ota 需要前者
	// （设备的 esp_http_client 不解 chunked）。
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, st.ModTime(), f)
}
