// Package photo 处理用户上传的照片：解码、方向校正、缩放到 1080p、
// 剥掉全部元数据、重新编码为 baseline JPEG，并对最终字节做内容寻址。
package photo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // 注册 PNG 解码
	"time"

	"golang.org/x/image/draw"
)

const (
	TargetWidth  = 1920
	TargetHeight = 1080
	// MaxStoredBytes 是单张最终 JPEG 的上限；压不到就拒绝，而不是无限降质量。
	MaxStoredBytes = 250 << 10
	// MaxUploadBytes 是原始上传体上限。
	MaxUploadBytes = 20 << 20
	// MaxPixels 防解压炸弹；手机 48MP 也在这个范围内。
	MaxPixels = 40_000_000
	// MinQuality 是自适应质量的下限。
	MinQuality = 40
)

var (
	ErrUnsupported    = errors.New("photo: 不支持的图片格式")
	ErrDecode         = errors.New("photo: 图片解码失败")
	ErrTooManyPixels  = errors.New("photo: 图片像素过多")
	ErrCannotCompress = errors.New("photo: 无法压到 250KB")
	ErrBadDuration    = errors.New("photo: duration_s 超出 5–600")
)

// Processed 是处理后的最终 JPEG。
type Processed struct {
	JPEG   []byte
	SHA256 string
	Size   int64
	Width  int
	Height int
}

// Process 对上传字节做全套处理。contentType 只用于把 HEIC 等格式的错误
// 区分成 415；真正的格式判断以字节为准。
func Process(data []byte, contentType string) (Processed, error) {
	format, px, err := sniff(data, contentType)
	if err != nil {
		return Processed{}, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Processed{}, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	if px > MaxPixels {
		return Processed{}, ErrTooManyPixels
	}
	orientation := 1
	if format == "jpeg" {
		orientation = jpegOrientation(data)
	}
	img = applyOrientation(img, orientation)
	canvas := fitBlack(img)

	out, err := encodeAdaptive(canvas)
	if err != nil {
		return Processed{}, err
	}
	sum := sha256.Sum256(out)
	return Processed{
		JPEG:   out,
		SHA256: hex.EncodeToString(sum[:]),
		Size:   int64(len(out)),
		Width:  TargetWidth,
		Height: TargetHeight,
	}, nil
}

// sniff 判断格式与像素数。只接受 JPEG/PNG；HEIC/GIF/动画直接拒绝。
func sniff(data []byte, contentType string) (format string, pixels int, err error) {
	if isHEIC(data, contentType) {
		return "", 0, ErrUnsupported
	}
	// GIF（含动图）、WebP 等一律不支持；不要等到 DecodeConfig 报“未知格式”，
	// 那会被当成坏图（400）而不是格式不支持（415）。
	if len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a") {
		return "", 0, ErrUnsupported
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "", 0, ErrUnsupported
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		if contentType == "image/heic" || contentType == "image/heif" {
			return "", 0, ErrUnsupported
		}
		return "", 0, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	switch format {
	case "jpeg":
	case "png":
		// APNG 带 acTL 块；Go 只解第一帧，动态照片不是我们要的语义。
		if bytes.Contains(data, []byte("acTL")) {
			return "", 0, ErrUnsupported
		}
	default:
		return "", 0, ErrUnsupported
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return "", 0, fmt.Errorf("%w: 尺寸非法", ErrDecode)
	}
	// 先粗算；真正解码后再用实际尺寸再查一次。
	return format, cfg.Width * cfg.Height, nil
}

func isHEIC(data []byte, contentType string) bool {
	if contentType == "image/heic" || contentType == "image/heif" {
		return true
	}
	for _, brand := range []string{"ftypheic", "ftypheix", "ftyphevc", "ftypmif1", "ftypmsf1"} {
		if bytes.Contains(data[:min(len(data), 64)], []byte(brand)) {
			return true
		}
	}
	return false
}

// applyOrientation 按 EXIF Orientation 1..8 变换像素。其他值按 1 处理。
func applyOrientation(src image.Image, o int) *image.RGBA {
	in := toRGBA(src)
	w, h := in.Bounds().Dx(), in.Bounds().Dy()
	if o < 2 || o > 8 {
		return in
	}
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch o {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			out.SetRGBA(x, y, in.RGBAAt(sx, sy))
		}
	}
	return out
}

func toRGBA(src image.Image) *image.RGBA {
	if r, ok := src.(*image.RGBA); ok {
		return r
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// fitBlack 把图片等比缩进 1920×1080，不裁切、不放大，四周补纯黑。
func fitBlack(src image.Image) *image.RGBA {
	canvas := image.NewRGBA(image.Rect(0, 0, TargetWidth, TargetHeight))
	draw.Draw(canvas, canvas.Bounds(),
		&image.Uniform{C: color.RGBA{0, 0, 0, 255}}, image.Point{}, draw.Src)

	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	if sw <= 0 || sh <= 0 {
		return canvas
	}
	scale := float64(TargetWidth) / float64(sw)
	if s := float64(TargetHeight) / float64(sh); s < scale {
		scale = s
	}
	if scale > 1 {
		scale = 1
	}
	dw := int(float64(sw)*scale + 0.5)
	dh := int(float64(sh)*scale + 0.5)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	x0 := (TargetWidth - dw) / 2
	y0 := (TargetHeight - dh) / 2
	dr := image.Rect(x0, y0, x0+dw, y0+dh)
	draw.CatmullRom.Scale(canvas, dr, src, src.Bounds(), draw.Over, nil)
	return canvas
}

// encodeAdaptive 从 q90 逐步降到 q40，取第一个 ≤250KB 的结果。
// Go 的 jpeg 编码器输出 baseline、4:2:0，且不写 EXIF/ICC/XMP。
func encodeAdaptive(img image.Image) ([]byte, error) {
	var last []byte
	for q := 90; q >= MinQuality; q -= 5 {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDecode, err)
		}
		last = buf.Bytes()
		if len(last) <= MaxStoredBytes {
			return last, nil
		}
	}
	return nil, ErrCannotCompress
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Now 由 service 注入，测试可固定时间。
var now = time.Now
