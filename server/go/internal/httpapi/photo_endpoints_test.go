package httpapi

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dayuer/stickbox/server/go/internal/photo"
)

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{40, 120, 200, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func postRaw(h http.Handler, path string, body []byte, contentType string,
	hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestPhotoUpload(t *testing.T) {
	h, dep := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")
	dep.api.SetPhotos(photo.New(dep.cfg.Root, dep.store, "quota"))

	data := jpegBytes(t, 800, 600)
	rr := postRaw(h, "/api/photo?show=1&secs=8", data, "image/jpeg", bearer(agentTok))
	if rr.Code != 201 {
		t.Fatalf("上传 = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		SHA256     string `json:"sha256"`
		Size       int64  `json:"size"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		PLV        int64  `json:"plv"`
		InPlaylist bool   `json:"in_playlist"`
	}
	if err := jsonDecode(rr.Body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.SHA256) != 64 || out.Size == 0 || out.Width != 1920 ||
		out.Height != 1080 || !out.InPlaylist || out.PLV != 1 {
		t.Fatalf("响应 = %+v", out)
	}
	// 文件存在且大小与响应一致。
	f, err := os.Open(filepath.Join(dep.cfg.Root, "photos",
		userIDOf(t, dep), out.SHA256+".jpg"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	if st.Size() != out.Size {
		t.Fatalf("文件大小 %d != %d", st.Size(), out.Size)
	}

	// 重复上传：200，不重复占配额；清单不重复。
	rr = postRaw(h, "/api/photo?show=1", data, "image/jpeg", bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("重复上传 = %d %s", rr.Code, rr.Body)
	}
}

func TestPhotoUploadShowZero(t *testing.T) {
	h, dep := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")
	dep.api.SetPhotos(photo.New(dep.cfg.Root, dep.store, "quota"))

	rr := postRaw(h, "/api/photo?show=0", jpegBytes(t, 300, 300), "image/jpeg", bearer(agentTok))
	if rr.Code != 201 {
		t.Fatalf("上传 = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		InPlaylist bool  `json:"in_playlist"`
		PLV        int64 `json:"plv"`
	}
	jsonDecode(rr.Body, &out)
	if out.InPlaylist || out.PLV != 0 {
		t.Fatalf("show=0 响应 = %+v", out)
	}
}

func TestPhotoUploadRejects(t *testing.T) {
	h, dep := newTestAPI(t)
	devTok, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")
	dep.api.SetPhotos(photo.New(dep.cfg.Root, dep.store, "quota"))
	data := jpegBytes(t, 100, 100)

	if rr := postRaw(h, "/api/photo", data, "image/jpeg", nil); rr.Code != 401 {
		t.Errorf("无令牌 = %d", rr.Code)
	}
	if rr := postRaw(h, "/api/photo", data, "image/jpeg", bearer(devTok)); rr.Code != 401 {
		t.Errorf("device 令牌 = %d", rr.Code)
	}
	if rr := postRaw(h, "/api/photo", data, "image/heic", bearer(agentTok)); rr.Code != 415 {
		t.Errorf("heic content-type = %d", rr.Code)
	}
	if rr := postRaw(h, "/api/photo", []byte("nope"), "image/jpeg", bearer(agentTok)); rr.Code != 400 {
		t.Errorf("坏图 = %d", rr.Code)
	}
	if rr := postRaw(h, "/api/photo?show=2", data, "image/jpeg", bearer(agentTok)); rr.Code != 400 {
		t.Errorf("show=2 = %d", rr.Code)
	}
	if rr := postRaw(h, "/api/photo?secs=1", data, "image/jpeg", bearer(agentTok)); rr.Code != 400 {
		t.Errorf("secs=1 = %d", rr.Code)
	}
	// 没有 Content-Length（用无 body 的请求模拟）。
	req := httptest.NewRequest(http.MethodPost, "/api/photo", nil)
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("Authorization", "Bearer "+agentTok)
	req.ContentLength = 0
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Errorf("无 Content-Length = %d", rr.Code)
	}
}

func userIDOf(t *testing.T, dep *testDeps) string {
	t.Helper()
	users, err := dep.store.ListUsers()
	if err != nil || len(users) == 0 {
		t.Fatalf("用户 = %v err=%v", users, err)
	}
	return users[0].ID
}

func jsonDecode(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }
