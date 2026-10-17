package jpegrestart

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r := uint8(x * 255 / (w - 1))
			g := uint8(y * 255 / (h - 1))
			b := uint8((x + y) * 255 / (w + h - 2))
			img.SetRGBA(x, y, color.RGBA{r, g, b, 255})
		}
	}
	return img
}

// scanMarkers 统计熵编码区里的 RST 数量，并返回 DRI 值。
func scanMarkers(t *testing.T, b []byte) (dri int, rst int) {
	t.Helper()
	for i := 0; i+1 < len(b); {
		if b[i] != 0xFF {
			i++
			continue
		}
		m := b[i+1]
		switch {
		case m == 0x00 || m == 0xFF:
			i += 2
		case m == 0xD8 || m == 0xD9 || (m >= 0xD0 && m <= 0xD7) || m == 0x01:
			if m >= 0xD0 && m <= 0xD7 {
				rst++
			}
			i += 2
		case m == 0xDA:
			// SOS：段头之后是熵编码区
			l := int(b[i+2])<<8 | int(b[i+3])
			i += 2 + l
			// 剩余部分里数 RST
			for j := i; j+1 < len(b); {
				if b[j] == 0xFF && b[j+1] >= 0xD0 && b[j+1] <= 0xD7 {
					rst++
					j += 2
					continue
				}
				if b[j] == 0xFF && b[j+1] == 0xD9 {
					return
				}
				j++
			}
			return
		default:
			l := int(b[i+2])<<8 | int(b[i+3])
			if m == 0xDD && l == 4 {
				dri = int(b[i+4])<<8 | int(b[i+5])
			}
			i += 2 + l
		}
	}
	return
}

func TestEncodeDRI(t *testing.T) {
	const w, h = 640, 192 // 40×12 MCU
	img := gradient(w, h)
	res, err := Encode(img, Options{Quality: 90, RestartInterval: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.MCUs) != 40*12 {
		t.Fatalf("MCU 数 = %d，要 %d", len(res.MCUs), 40*12)
	}
	dri, rst := scanMarkers(t, res.JPEG)
	if dri != 8 {
		t.Errorf("DRI = %d，要 8", dri)
	}
	// 每行 5 个 interval，12 行；最后一个 interval 不补 RST
	if want := 40*12/8 - 1; rst != want {
		t.Errorf("RST 数 = %d，要 %d", rst, want)
	}

	decoded, err := jpeg.Decode(bytes.NewReader(res.JPEG))
	if err != nil {
		t.Fatalf("标准库解码失败: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("解码尺寸 = %v", b)
	}
	assertClose(t, img, decoded, 40)
}

func TestEncodeFinalRST(t *testing.T) {
	img := gradient(128, 192) // 8×12 MCU，每行一个 interval
	res, err := Encode(img, Options{Quality: 90, RestartInterval: 8, FinalRST: true})
	if err != nil {
		t.Fatal(err)
	}
	_, rst := scanMarkers(t, res.JPEG)
	if rst != 12 {
		t.Errorf("FinalRST 时 RST 数 = %d，要 12（每行一个，含最后一行）", rst)
	}
	if _, err := jpeg.Decode(bytes.NewReader(res.JPEG)); err != nil {
		t.Fatalf("标准库解码失败（最后一个 interval 后多一个 RST）: %v", err)
	}
}

func assertClose(t *testing.T, want, got image.Image, maxDiff int) {
	t.Helper()
	b := want.Bounds()
	if got.Bounds() != b {
		t.Fatalf("尺寸不同: %v vs %v", b, got.Bounds())
	}
	sum, worst := 0, 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r1, g1, b1, _ := want.At(x, y).RGBA()
			r2, g2, b2, _ := got.At(x, y).RGBA()
			for _, d := range []int{
				abs(int(r1>>8) - int(r2>>8)),
				abs(int(g1>>8) - int(g2>>8)),
				abs(int(b1>>8) - int(b2>>8)),
			} {
				sum += d
				if d > worst {
					worst = d
				}
			}
		}
	}
	n := b.Dx() * b.Dy() * 3
	mean := float64(sum) / float64(n)
	if worst > maxDiff {
		t.Errorf("最大像素差 = %d，超过 %d（平均 %.2f）", worst, maxDiff, mean)
	}
	if mean > 6 {
		t.Errorf("平均像素差 = %.2f，太大", mean)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
