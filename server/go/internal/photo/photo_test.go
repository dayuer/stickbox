package photo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dayuer/stickbox/server/go/internal/store"
)

func solid(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func halfRedBlue(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/2 {
				img.SetRGBA(x, y, color.RGBA{220, 30, 30, 255})
			} else {
				img.SetRGBA(x, y, color.RGBA{30, 30, 220, 255})
			}
		}
	}
	return img
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// 在 SOI 后插入一个最小的 EXIF APP1，只带 Orientation。
func withOrientation(jpg []byte, o int) []byte {
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0,
		1, 0, // 1 entry
		0x12, 0x01, 3, 0, 1, 0, 0, 0, byte(o), 0, 0, 0,
		0, 0, 0, 0}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	out = append(out, payload...)
	out = append(out, jpg[2:]...)
	return out
}

func decodeOut(t *testing.T, p Processed) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(p.JPEG))
	if err != nil {
		t.Fatalf("输出不是合法图片：%v", err)
	}
	return img
}

func isRed(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	return r > 0x9000 && b < 0x6000 && g < 0x6000
}
func isBlue(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	return b > 0x9000 && r < 0x6000 && g < 0x6000
}
func isBlack(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	return r < 0x1000 && g < 0x1000 && b < 0x1000
}

func TestProcessScalesAndLetterboxes(t *testing.T) {
	p, err := Process(encodeJPEG(t, solid(4000, 1000, color.RGBA{200, 40, 40, 255})), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if p.Width != 1920 || p.Height != 1080 || p.Size > MaxStoredBytes {
		t.Fatalf("结果 = %+v", p)
	}
	img := decodeOut(t, p)
	if !isBlack(img.At(960, 100)) {
		t.Errorf("顶部留白不是黑：%v", img.At(960, 100))
	}
	if !isRed(img.At(960, 540)) {
		t.Errorf("中心不是照片内容：%v", img.At(960, 540))
	}
}

// Orientation 6 = 顺时针 90°；原图左红右蓝，转完应上红下蓝。
func TestProcessAppliesEXIFOrientation(t *testing.T) {
	jpg := withOrientation(encodeJPEG(t, halfRedBlue(400, 200)), 6)
	p, err := Process(jpg, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	img := decodeOut(t, p)
	// 200×400 居中：x 860..1060, y 340..740。
	if !isRed(img.At(960, 400)) {
		t.Errorf("上半部分 = %v，期望红（顺时针旋转后左半边在上）", img.At(960, 400))
	}
	if !isBlue(img.At(960, 680)) {
		t.Errorf("下半部分 = %v，期望蓝", img.At(960, 680))
	}
}

func TestProcessStripsMetadata(t *testing.T) {
	jpg := withOrientation(encodeJPEG(t, solid(300, 200, color.RGBA{10, 200, 10, 255})), 1)
	// 再塞一个 COM 段模拟 GPS 文本。
	com := []byte("GPS_LAT_12.3456")
	seg := append([]byte{0xFF, 0xFE, byte((len(com) + 2) >> 8), byte(len(com) + 2)}, com...)
	jpg = append(append([]byte{}, jpg[:2]...), append(seg, jpg[2:]...)...)

	p, err := Process(jpg, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(p.JPEG, []byte("Exif")) || bytes.Contains(p.JPEG, []byte("GPS_LAT")) {
		t.Fatal("输出里仍带元数据")
	}
}

func TestProcessPNGAlphaOnBlack(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 200, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 200; x++ {
			img.SetNRGBA(x, y, color.NRGBA{255, 0, 0, 0}) // 全透明
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	p, err := Process(b.Bytes(), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	imgOut := decodeOut(t, p)
	if !isBlack(imgOut.At(960, 540)) {
		t.Errorf("透明像素没有合成到黑底：%v", imgOut.At(960, 540))
	}
}

func TestProcessRejectsUnsupported(t *testing.T) {
	gif := []byte("GIF89a" + strings.Repeat("\x00", 64))
	if _, err := Process(gif, "image/gif"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("GIF err = %v", err)
	}
	heic := append([]byte("\x00\x00\x00\x18ftypheic"), make([]byte, 64)...)
	if _, err := Process(heic, "image/heic"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("HEIC err = %v", err)
	}
	if _, err := Process([]byte("not an image"), "image/jpeg"); !errors.Is(err, ErrDecode) {
		t.Fatalf("垃圾数据 err = %v", err)
	}
}

// 随机噪声是最坏情况：q40 也压不到 250KB 时必须明确拒绝，不能无限降质量。
func TestProcessRejectsIncompressible(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	seed := uint32(1)
	for y := 0; y < 1080; y++ {
		for x := 0; x < 1920; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{byte(seed >> 24), byte(seed >> 16), byte(seed >> 8), 255})
		}
	}
	if _, err := Process(encodeJPEG(t, img), "image/jpeg"); !errors.Is(err, ErrCannotCompress) {
		t.Fatalf("err = %v，期望 ErrCannotCompress", err)
	}
}

// —— service ——

type fakeStore struct {
	created     bool
	addErr      error
	addCalls    int
	appendCalls int
	appendSHA   string
	appendDur   int
	plv         int64
	playlist    []store.PlaylistEntry
	init        store.PhotoInit
	initOK      bool
	initDone    map[string]bool
}

func (f *fakeStore) AddPhoto(string, store.Photo, int, int64) (bool, int, int64, error) {
	f.addCalls++
	return f.created, 1, 0, f.addErr
}
func (f *fakeStore) PhotoBySHA(string, string) (store.Photo, bool, error) {
	return store.Photo{}, false, nil
}
func (f *fakeStore) AppendPhotoToPlaylist(_, sha, _ string, dur int) (int64, bool, error) {
	f.appendCalls++
	f.appendSHA, f.appendDur = sha, dur
	return f.plv, true, nil
}
func (f *fakeStore) PhotosOfUser(string) ([]store.Photo, error) { return nil, nil }
func (f *fakeStore) Playlist(string) (int64, []store.PlaylistEntry, error) {
	return f.plv, f.playlist, nil
}
func (f *fakeStore) ReplacePlaylist(_ string, e []store.PlaylistEntry) (int64, error) {
	f.playlist = e
	f.plv++
	return f.plv, nil
}
func (f *fakeStore) DeletePhotoWithPlaylist(string, string) (int64, bool, error) {
	return f.plv, false, nil
}
func (f *fakeStore) BumpPlaylistVersion(string) (int64, error) {
	f.plv++
	return f.plv, nil
}
func (f *fakeStore) SetPhotoInit(_ string, in store.PhotoInit) error {
	f.init, f.initOK = in, true
	return nil
}
func (f *fakeStore) PhotoInit(string) (store.PhotoInit, bool, error) {
	return f.init, f.initOK, nil
}
func (f *fakeStore) ClearPhotoInit(string) error {
	f.initOK = false
	return nil
}
func (f *fakeStore) InitDoneForUser(_, prefix string) (bool, error) {
	return f.initDone[prefix], nil
}

func TestUploadWritesFileAndAppends(t *testing.T) {
	root := t.TempDir()
	st := &fakeStore{created: true, plv: 7}
	svc := New(root, st, "quota")
	res, err := svc.Upload("u1", encodeJPEG(t, solid(300, 200, color.RGBA{1, 2, 3, 255})),
		"image/jpeg", true, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.PLV != 7 || !res.InPlaylist {
		t.Fatalf("结果 = %+v", res)
	}
	raw, err := os.ReadFile(filepath.Join(root, "photos", "u1", res.SHA256+".jpg"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != res.SHA256 || int64(len(raw)) != res.Size {
		t.Fatalf("文件哈希/大小与结果不一致")
	}
	if st.addCalls != 1 || st.appendCalls != 1 || st.appendDur != 8 ||
		st.appendSHA != res.SHA256 {
		t.Fatalf("存储调用 = add:%d append:%d dur:%d sha:%s",
			st.addCalls, st.appendCalls, st.appendDur, st.appendSHA)
	}
}

func TestUploadQuotaError(t *testing.T) {
	svc := New(t.TempDir(), &fakeStore{addErr: store.ErrPhotoQuota}, "quota")
	_, err := svc.Upload("u1", encodeJPEG(t, solid(100, 100, color.RGBA{9, 9, 9, 255})),
		"image/jpeg", false, 8)
	if !errors.Is(err, store.ErrPhotoQuota) {
		t.Fatalf("err = %v", err)
	}
}
