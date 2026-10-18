package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/photo"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

func setupPhotos(t *testing.T) (http.Handler, *testDeps, string, string) {
	t.Helper()
	h, dep := newTestAPI(t)
	devTok, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")
	dep.api.SetPhotos(photo.New(dep.cfg.Root, dep.store, "quota"))
	return h, dep, agentTok, devTok
}

func uploadPhoto(t *testing.T, h http.Handler, tok string, show bool) (sha string, size int64) {
	return uploadPhotoVariant(t, h, tok, show, 0)
}

func uploadPhotoVariant(t *testing.T, h http.Handler, tok string, show bool, variant int) (sha string, size int64) {
	t.Helper()
	path := "/api/photo?show=0"
	if show {
		path = "/api/photo?show=1&secs=8"
	}
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			img.SetRGBA(x, y, color.RGBA{byte(30 + variant*40), byte(80 + x%120), byte(160 + y%80), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	rr := postRaw(h, path, buf.Bytes(), "image/jpeg", bearer(tok))
	if rr.Code != 201 && rr.Code != 200 {
		t.Fatalf("上传 = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	json.NewDecoder(rr.Body).Decode(&out)
	return out.SHA256, out.Size
}

func getJSON(h http.Handler, path string, hdr map[string]string, out any) *httptest.ResponseRecorder {
	rr := get(h, path, hdr)
	if out != nil && rr.Code == 200 {
		json.NewDecoder(rr.Body).Decode(out)
	}
	return rr
}

func uploadBigPhoto(t *testing.T, h http.Handler, tok string) (sha string, size int64) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	seed := uint32(7)
	for by := 0; by < 1080; by += 16 {
		for bx := 0; bx < 1920; bx += 16 {
			seed = seed*1664525 + 1013904223
			c := color.RGBA{byte(seed >> 24), byte(seed >> 16), byte(seed >> 8), 255}
			for y := by; y < by+16 && y < 1080; y++ {
				for x := bx; x < bx+16 && x < 1920; x++ {
					img.SetRGBA(x, y, c)
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	rr := postRaw(h, "/api/photo?show=0", buf.Bytes(), "image/jpeg", bearer(tok))
	if rr.Code != 201 && rr.Code != 200 {
		t.Fatalf("大图上传 = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	json.NewDecoder(rr.Body).Decode(&out)
	return out.SHA256, out.Size
}

func TestDevicePlaylistManifest(t *testing.T) {
	h, _, agentTok, devTok := setupPhotos(t)
	sha, size := uploadPhoto(t, h, agentTok, true)

	var m struct {
		PLV     int64 `json:"plv"`
		Entries []struct {
			Kind      string `json:"kind"`
			Theme     string `json:"theme"`
			SHA256    string `json:"sha256"`
			Size      int64  `json:"size"`
			DurationS int    `json:"duration_s"`
			Order     int    `json:"order"`
			URL       string `json:"url"`
		} `json:"entries"`
	}
	rr := getJSON(h, "/api/device/playlist", bearer(devTok), &m)
	if rr.Code != 200 {
		t.Fatalf("GET = %d %s", rr.Code, rr.Body)
	}
	if rr.Header().Get("ETag") != `"1"` {
		t.Fatalf("ETag = %q", rr.Header().Get("ETag"))
	}
	if m.PLV != 1 || len(m.Entries) != 2 {
		t.Fatalf("manifest = %+v", m)
	}
	if m.Entries[0].Kind != "board" || m.Entries[0].Theme != "quota" || m.Entries[0].Order != 0 {
		t.Fatalf("board = %+v", m.Entries[0])
	}
	p := m.Entries[1]
	if p.Kind != "photo" || p.SHA256 != sha || p.Size != size ||
		p.DurationS != 8 || p.Order != 1 || p.URL != "/api/device/photo/"+sha {
		t.Fatalf("photo = %+v", p)
	}

	var noInit struct {
		InitToken     string `json:"init_token"`
		InitForce     *bool  `json:"init_force"`
		InitExpiresAt int64  `json:"init_expires_at"`
	}
	getJSON(h, "/api/device/playlist", bearer(devTok), &noInit)
	if noInit.InitToken != "" || noInit.InitForce != nil || noInit.InitExpiresAt != 0 {
		t.Fatalf("无授权时不应有 init 字段：%+v", noInit)
	}

	// 304。
	rr = get(h, "/api/device/playlist", map[string]string{
		"Authorization": "Bearer " + devTok, "If-None-Match": `"1"`})
	if rr.Code != http.StatusNotModified {
		t.Fatalf("304 = %d", rr.Code)
	}

	// 老固件 device 令牌以外的角色拿不到。
	if rr := get(h, "/api/device/playlist", bearer(agentTok)); rr.Code != 401 {
		t.Fatalf("agent 取清单 = %d", rr.Code)
	}
}

func TestPlaylistPutAndDelete(t *testing.T) {
	h, _, agentTok, devTok := setupPhotos(t)
	sha1, _ := uploadPhotoVariant(t, h, agentTok, false, 1)
	sha2, _ := uploadPhotoVariant(t, h, agentTok, false, 2)

	body := fmt.Sprintf(`{"entries":[
		{"kind":"board","theme":"quota","duration_s":20},
		{"kind":"photo","sha256":%q,"duration_s":5},
		{"kind":"photo","sha256":%q,"duration_s":9}]}`, sha2, sha1)
	rr := put(h, "/api/playlist", body, bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	var m struct {
		PLV     int64 `json:"plv"`
		Entries []struct {
			Kind      string `json:"kind"`
			SHA256    string `json:"sha256"`
			DurationS int    `json:"duration_s"`
			Order     int    `json:"order"`
		} `json:"entries"`
	}
	json.NewDecoder(rr.Body).Decode(&m)
	if m.PLV != 1 || len(m.Entries) != 3 || m.Entries[1].SHA256 != sha2 ||
		m.Entries[1].DurationS != 5 || m.Entries[2].Order != 2 {
		t.Fatalf("PUT 响应 = %+v", m)
	}

	// agent 照片列表显示在清单里与时长。
	var list struct {
		Photos []struct {
			SHA256     string `json:"sha256"`
			InPlaylist bool   `json:"in_playlist"`
			DurationS  int    `json:"duration_s"`
		} `json:"photos"`
	}
	getJSON(h, "/api/agent/photos", bearer(agentTok), &list)
	if len(list.Photos) != 2 {
		t.Fatalf("photos = %+v", list.Photos)
	}

	// 删除 sha2：清单重排、plv 前进。
	rr = do(h, httptest.NewRequest(http.MethodDelete, "/api/photo/"+sha2, nil), bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("DELETE = %d %s", rr.Code, rr.Body)
	}
	getJSON(h, "/api/device/playlist", bearer(devTok), &m)
	if len(m.Entries) != 2 || m.Entries[1].SHA256 != sha1 || m.Entries[1].Order != 1 {
		t.Fatalf("删除后 = %+v", m.Entries)
	}
	// 再删一次 404。
	rr = do(h, httptest.NewRequest(http.MethodDelete, "/api/photo/"+sha2, nil), bearer(agentTok))
	if rr.Code != 404 {
		t.Fatalf("重复删除 = %d", rr.Code)
	}
}

func TestPhotoInitTokenManifest(t *testing.T) {
	h, dep, agentTok, devTok := setupPhotos(t)
	rr := post(h, "/api/photo/init", `{"force":true}`, bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("init = %d %s", rr.Code, rr.Body)
	}
	var init struct {
		Token     string `json:"init_token"`
		Force     bool   `json:"force"`
		ExpiresAt int64  `json:"expires_at"`
	}
	json.NewDecoder(rr.Body).Decode(&init)
	if len(init.Token) < 16 || !init.Force || init.ExpiresAt == 0 {
		t.Fatalf("init 响应 = %+v", init)
	}

	var m struct {
		InitToken     string `json:"init_token"`
		InitForce     *bool  `json:"init_force"`
		InitExpiresAt int64  `json:"init_expires_at"`
	}
	getJSON(h, "/api/device/playlist", bearer(devTok), &m)
	if m.InitToken != init.Token || m.InitForce == nil || !*m.InitForce ||
		m.InitExpiresAt != init.ExpiresAt {
		t.Fatalf("manifest token = %+v", m)
	}

	// 设备上报 init_done 后（此处直接写库模拟 worker），token 整组消失。
	userID := userIDOf(t, dep)
	if err := dep.store.PutDevicePhoto("f412fa87c9e0", store.DevicePhoto{
		UserID: userID, InitDone: init.Token[:8], Updated: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	var used struct {
		InitToken     string `json:"init_token"`
		InitForce     *bool  `json:"init_force"`
		InitExpiresAt int64  `json:"init_expires_at"`
	}
	getJSON(h, "/api/device/playlist", bearer(devTok), &used)
	if used.InitToken != "" || used.InitForce != nil || used.InitExpiresAt != 0 {
		t.Fatalf("用掉后仍带 token = %+v", used)
	}

	// 过期 token 不下发。
	if err := dep.store.SetPhotoInit(userID, store.PhotoInit{
		Token: "deadbeefdeadbeef", Expires: time.Now().Add(-time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	var expired struct {
		InitToken     string `json:"init_token"`
		InitForce     *bool  `json:"init_force"`
		InitExpiresAt int64  `json:"init_expires_at"`
	}
	getJSON(h, "/api/device/playlist", bearer(devTok), &expired)
	if expired.InitToken != "" || expired.InitForce != nil || expired.InitExpiresAt != 0 {
		t.Fatalf("过期 token 仍下发 = %+v", expired)
	}

	// 普通 init（非 force）：三字段齐全，init_force 显式为 false。
	rr = post(h, "/api/photo/init", `{}`, bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("init = %d %s", rr.Code, rr.Body)
	}
	var normal struct {
		InitToken     string `json:"init_token"`
		InitForce     *bool  `json:"init_force"`
		InitExpiresAt int64  `json:"init_expires_at"`
	}
	getJSON(h, "/api/device/playlist", bearer(devTok), &normal)
	if normal.InitToken == "" || normal.InitForce == nil || *normal.InitForce ||
		normal.InitExpiresAt == 0 {
		t.Fatalf("普通 init manifest = %+v", normal)
	}
}

func TestDevicePhotoRange(t *testing.T) {
	h, _, agentTok, devTok := setupPhotos(t)
	sha, size := uploadBigPhoto(t, h, agentTok)
	if size < 50_000 {
		t.Fatalf("样本太小：%d", size)
	}
	path := "/api/device/photo/" + sha

	// 完整下载。
	rr := get(h, path, bearer(devTok))
	if rr.Code != 200 || int64(rr.Body.Len()) != size ||
		rr.Header().Get("Content-Length") != fmt.Sprint(size) ||
		rr.Header().Get("ETag") != `"`+sha+`"` ||
		rr.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("完整下载 = %d len%d headers=%v", rr.Code, rr.Body.Len(), rr.Header())
	}
	// 304。
	rr = get(h, path, map[string]string{
		"Authorization": "Bearer " + devTok, "If-None-Match": `"` + sha + `"`})
	if rr.Code != 304 {
		t.Fatalf("304 = %d", rr.Code)
	}
	// bytes=0-9。
	rr = get(h, path, map[string]string{
		"Authorization": "Bearer " + devTok, "Range": "bytes=0-9"})
	if rr.Code != 206 || rr.Body.Len() != 10 ||
		rr.Header().Get("Content-Range") != fmt.Sprintf("bytes 0-9/%d", size) {
		t.Fatalf("0-9 = %d len%d cr=%q", rr.Code, rr.Body.Len(), rr.Header().Get("Content-Range"))
	}
	// bytes=10- 到结尾。
	rr = get(h, path, map[string]string{
		"Authorization": "Bearer " + devTok, "Range": "bytes=10-"})
	if rr.Code != 206 || int64(rr.Body.Len()) != size-10 ||
		rr.Header().Get("Content-Range") != fmt.Sprintf("bytes 10-%d/%d", size-1, size) {
		t.Fatalf("10- = %d len%d", rr.Code, rr.Body.Len())
	}
	// 越界、多段、后缀形式都 416。
	for _, rng := range []string{
		fmt.Sprintf("bytes=%d-", size), "bytes=0-1,3-4", "bytes=-100", "items=0-1",
	} {
		rr = get(h, path, map[string]string{
			"Authorization": "Bearer " + devTok, "Range": rng})
		if rr.Code != 416 || rr.Header().Get("Content-Range") != fmt.Sprintf("bytes */%d", size) {
			t.Errorf("Range %q = %d cr=%q", rng, rr.Code, rr.Header().Get("Content-Range"))
		}
	}
}

func TestAgentDevicesIncludesPhotoState(t *testing.T) {
	h, dep, agentTok, _ := setupPhotos(t)
	userID := userIDOf(t, dep)
	if err := dep.store.PutDevicePhoto("f412fa87c9e0", store.DevicePhoto{
		UserID: userID, Cap: true, Ready: true, State: "ok", PLV: 7, Have: 12,
		Bad: []string{"a1b2c3d4"}, FreeKB: 1024, InitDone: "abcdef01",
	}); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Devices []struct {
			Dev   string `json:"dev"`
			Photo *struct {
				Cap      bool     `json:"cap"`
				Ready    bool     `json:"ready"`
				State    string   `json:"state"`
				PLV      int64    `json:"plv"`
				Have     int      `json:"have"`
				Bad      []string `json:"bad"`
				FreeKB   int64    `json:"free_kb"`
				InitDone string   `json:"init_done"`
			} `json:"photo"`
		} `json:"devices"`
	}
	getJSON(h, "/api/agent/devices", bearer(agentTok), &out)
	if len(out.Devices) != 1 || out.Devices[0].Photo == nil {
		t.Fatalf("devices = %+v", out)
	}
	p := out.Devices[0].Photo
	if !p.Cap || !p.Ready || p.PLV != 7 || p.Have != 12 ||
		len(p.Bad) != 1 || p.InitDone != "abcdef01" {
		t.Fatalf("photo = %+v", p)
	}
}
