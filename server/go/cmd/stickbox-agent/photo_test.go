package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupAgentConfig(t *testing.T, server string) {
	t.Helper()
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	if err := saveConfig(&agentConfig{Server: server, Token: "k.s"}); err != nil {
		t.Fatal(err)
	}
}

func stubPrepare(t *testing.T, data []byte) {
	t.Helper()
	old := preparePhoto
	preparePhoto = func(string) ([]byte, string, func(), string, error) {
		return data, "image/jpeg", func() {}, "", nil
	}
	t.Cleanup(func() { preparePhoto = old })
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b), runErr
}

func jpegFile(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.SetRGBA(x, y, color.RGBA{10, 200, 30, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "a.jpg")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPhotoAddShowAndNoShow(t *testing.T) {
	var queries, cts, auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/photo" || r.Method != "POST" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		queries = append(queries, r.URL.RawQuery)
		cts = append(cts, r.Header.Get("Content-Type"))
		auths = append(auths, r.Header.Get("Authorization"))
		var body bytes.Buffer
		body.ReadFrom(r.Body)
		if body.Len() == 0 || r.ContentLength != int64(body.Len()) {
			t.Errorf("body len=%d cl=%d", body.Len(), r.ContentLength)
		}
		w.WriteHeader(201)
		fmt.Fprint(w, `{"sha256":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789","size":12345,"width":1920,"height":1080,"plv":3,"in_playlist":true}`)
	}))
	defer srv.Close()
	setupAgentConfig(t, srv.URL)
	stubPrepare(t, []byte("jpeg-bytes"))
	file := jpegFile(t)

	out, err := captureStdout(t, func() error { return cmdPhoto([]string{"add", file}) })
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if !strings.Contains(out, "abcdef01") || !strings.Contains(out, "已上屏") {
		t.Fatalf("输出 = %s", out)
	}
	if !strings.Contains(queries[0], "show=1") || !strings.Contains(queries[0], "secs=8") {
		t.Fatalf("query = %s", queries[0])
	}
	if cts[0] != "image/jpeg" || auths[0] != "Bearer k.s" {
		t.Fatalf("headers = %q %q", cts[0], auths[0])
	}

	if _, err := captureStdout(t, func() error {
		return cmdPhoto([]string{"add", "--no-show", "--secs", "20", file})
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(queries[1], "show=0") || !strings.Contains(queries[1], "secs=20") {
		t.Fatalf("query = %s", queries[1])
	}
}

func TestPhotoListShowsSHAAndDeviceCard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent/photos":
			fmt.Fprint(w, `{"photos":[
				{"sha256":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
				 "size":198432,"width":1920,"height":1080,"uploaded_at":1700000000,
				 "in_playlist":true,"duration_s":8}]}`)
		case "/api/agent/devices":
			fmt.Fprint(w, `{"devices":[{"dev":"f412fa87c9e0","online":true,
				"photo":{"cap":true,"ready":true,"state":"ok","plv":7,"have":12,
				"bad":["a1b2c3d4"],"free_kb":1073741824,"init_done":"3f2a9c01"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	setupAgentConfig(t, srv.URL)

	out, err := captureStdout(t, func() error { return cmdPhoto([]string{"ls"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"abcdef01", "193.8KB", "8s", "在清单",
		"设备 f412fa87c9e0（在线）", "ready=true state=ok plv=7 已缓存=12",
		"跳过：a1b2c3d4", "已用初始化 token：3f2a9c01",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q：\n%s", want, out)
		}
	}
}

func TestPhotoRemoveByPrefix(t *testing.T) {
	full := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/agent/photos":
			fmt.Fprintf(w, `{"photos":[{"sha256":%q,"size":1,"in_playlist":true}]}`, full)
		case strings.HasPrefix(r.URL.Path, "/api/photo/"):
			deleted = strings.TrimPrefix(r.URL.Path, "/api/photo/")
			fmt.Fprint(w, `{"ok":1,"plv":9}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	setupAgentConfig(t, srv.URL)

	out, err := captureStdout(t, func() error { return cmdPhoto([]string{"rm", "abcdef01"}) })
	if err != nil || deleted != full || !strings.Contains(out, "已删除 abcdef01（plv 9）") {
		t.Fatalf("err=%v deleted=%q out=%s", err, deleted, out)
	}
	// 太短的前缀拒绝。
	if err := cmdPhoto([]string{"rm", "abcd"}); err == nil {
		t.Fatal("4 位前缀应拒绝")
	}
}

func TestPhotoInitConfirmationAndTokenRedaction(t *testing.T) {
	fullToken := "abcdef0123456789abcdef0123456789"
	var posts int
	var body map[string]bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/photo/init" || r.Method != "POST" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		posts++
		json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprintf(w, `{"init_token":%q,"force":true,"expires_at":1893456000,"plv":4}`, fullToken)
	}))
	defer srv.Close()
	setupAgentConfig(t, srv.URL)

	old := stdinReader
	stdinReader = strings.NewReader("yes\nyes\n")
	t.Cleanup(func() { stdinReader = old })

	out, err := captureStdout(t, func() error { return cmdPhoto([]string{"init", "--force"}) })
	if err != nil {
		t.Fatal(err)
	}
	if posts != 1 || !body["force"] {
		t.Fatalf("posts=%d body=%v", posts, body)
	}
	if !strings.Contains(out, "token abcdef01…") || !strings.Contains(out, "有效至") {
		t.Fatalf("输出 = %s", out)
	}
	if strings.Contains(out, fullToken) {
		t.Fatal("输出泄露了完整 init_token")
	}
	if strings.Count(out, "输入 yes") != 2 {
		t.Fatalf("--force 应有两次确认提示：%s", out)
	}
}

func TestPhotoInitCancel(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
	}))
	defer srv.Close()
	setupAgentConfig(t, srv.URL)

	old := stdinReader
	stdinReader = strings.NewReader("no\n")
	t.Cleanup(func() { stdinReader = old })

	out, err := captureStdout(t, func() error { return cmdPhoto([]string{"init"}) })
	if err != nil || posts != 0 || !strings.Contains(out, "已取消") {
		t.Fatalf("err=%v posts=%d out=%s", err, posts, out)
	}
}

// 真实 sips 只在 macOS 上跑；验证原图路径不会被改写、能读回字节。
func TestPreparePhotoFileLocal(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sips"); err != nil {
		t.Skip("没有 sips")
	}
	p := jpegFile(t)
	data, ct, cleanup, _, err := preparePhotoFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if ct != "image/jpeg" || len(data) == 0 {
		t.Fatalf("ct=%s len=%d", ct, len(data))
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("原图被动了")
	}
}
