// Package jpegrestart 是一个最小的 baseline JPEG 编码器，唯一目的：
// 支持 restart interval（DRI）。
//
// Go 标准库的 image/jpeg 不写 DRI/RST，而设备侧要把每张时钟卡片的
// MCU 当作独立字节段替换（方案 D，DRI=1），所以编码器必须自己写。
//
// 输出固定为：8 位、YCbCr 4:2:0、两张量化表、标准 4 张哈夫曼表，
// 与 rom/main/jband.h 的解析前提一致。
package jpegrestart

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"math"
)

// Options 控制编码。
type Options struct {
	Quality int // 1..100，默认 90
	// RestartInterval 是每隔多少个 MCU 一个 RST。方案 D 用 1。
	RestartInterval int
	// FinalRST 为 true 时，最后一个 MCU 后也写 RST（生成字形图集时用，
	// 保证每个可替换段都自带结尾标记）。
	FinalRST bool
}

// MCU 是熵编码流里一个 MCU 段的字节范围（相对整张 JPEG）。
// 段包含结尾的 RSTn（如果该 MCU 后有）。
type MCU struct {
	Offset int
	Length int
}

// Result 是编码结果与 MCU 索引。
type Result struct {
	JPEG []byte
	MCUs []MCU
}

const (
	blockSize = 64
	mcuSize   = 16
)

// Encode 编码一张图。
func Encode(m image.Image, o Options) (*Result, error) {
	if m == nil {
		return nil, fmt.Errorf("jpegrestart: nil image")
	}
	b := m.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("jpegrestart: empty image")
	}
	q := o.Quality
	if q <= 0 {
		q = 90
	}
	if q > 100 {
		q = 100
	}
	interval := o.RestartInterval
	if interval <= 0 {
		interval = 1
	}

	qZig, qNat := quantTables(q)
	dcLum, acLum, dcChr, acChr := huffmanTables()

	var out bytes.Buffer
	writeSOI(&out)
	writeAPP0(&out)
	writeDQT(&out, qZig)
	writeSOF0(&out, w, h)
	writeDHT(&out, dcLum, acLum, dcChr, acChr)
	writeDRI(&out, interval)
	writeSOS(&out)

	src := newSource(m)
	mcuCols := (w + mcuSize - 1) / mcuSize
	mcuRows := (h + mcuSize - 1) / mcuSize
	total := mcuCols * mcuRows

	bw := bitWriter{buf: &out}
	mcus := make([]MCU, 0, total)
	var predY, predCb, predCr int32
	rstNum := 0
	for my := 0; my < mcuRows; my++ {
		for mx := 0; mx < mcuCols; mx++ {
			start := out.Len()
			mcuIndex := my*mcuCols + mx
			// 每个 restart interval 起点 DC 预测清零。
			if mcuIndex%interval == 0 {
				predY, predCb, predCr = 0, 0, 0
			}

			var yb [4][blockSize]int32
			var cbb, crb [blockSize]int32
			var yf [4][blockSize]float64
			var cbf, crf [blockSize]float64

			for by := 0; by < 2; by++ {
				for bx := 0; bx < 2; bx++ {
					blk := by*2 + bx
					for yy := 0; yy < 8; yy++ {
						for xx := 0; xx < 8; xx++ {
							r, g, bb := src.rgb(mx*mcuSize+bx*8+xx, my*mcuSize+by*8+yy)
							yf[blk][yy*8+xx] = luma(r, g, bb) - 128
						}
					}
				}
			}
			for yy := 0; yy < 8; yy++ {
				for xx := 0; xx < 8; xx++ {
					var sr, sg, sb float64
					for dy := 0; dy < 2; dy++ {
						for dx := 0; dx < 2; dx++ {
							r, g, bb := src.rgb(mx*mcuSize+xx*2+dx, my*mcuSize+yy*2+dy)
							sr += r
							sg += g
							sb += bb
						}
					}
					sr, sg, sb = sr/4, sg/4, sb/4
					cbf[yy*8+xx] = cb(sr, sg, sb)
					crf[yy*8+xx] = cr(sr, sg, sb)
				}
			}

			for i := 0; i < 4; i++ {
				fdctQuant(&yf[i], &qNat[0], &yb[i])
				encodeBlock(&bw, &yb[i], &predY, dcLum, acLum)
			}
			fdctQuant(&cbf, &qNat[1], &cbb)
			encodeBlock(&bw, &cbb, &predCb, dcChr, acChr)
			fdctQuant(&crf, &qNat[1], &crb)
			encodeBlock(&bw, &crb, &predCr, dcChr, acChr)

			last := mcuIndex == total-1
			boundary := (mcuIndex+1)%interval == 0
			if (!last && boundary) || (last && o.FinalRST) {
				bw.flush()
				out.WriteByte(0xFF)
				out.WriteByte(0xD0 + byte(rstNum&7))
				rstNum++
			}

			end := out.Len()
			mcus = append(mcus, MCU{Offset: start, Length: end - start})
		}
	}
	bw.flush()
	out.Write([]byte{0xFF, 0xD9}) // EOI

	return &Result{JPEG: out.Bytes(), MCUs: mcus}, nil
}

// ---- 采样 ----

type pixelSource struct {
	m    image.Image
	rgba *image.RGBA
	min  image.Point
	max  image.Point
}

func newSource(m image.Image) pixelSource {
	s := pixelSource{m: m, min: m.Bounds().Min, max: m.Bounds().Max}
	if r, ok := m.(*image.RGBA); ok {
		s.rgba = r
	}
	return s
}

func (s pixelSource) rgb(x, y int) (float64, float64, float64) {
	if x < s.min.X {
		x = s.min.X
	}
	if y < s.min.Y {
		y = s.min.Y
	}
	if x >= s.max.X {
		x = s.max.X - 1
	}
	if y >= s.max.Y {
		y = s.max.Y - 1
	}
	if s.rgba != nil {
		i := s.rgba.PixOffset(x, y)
		return float64(s.rgba.Pix[i]), float64(s.rgba.Pix[i+1]), float64(s.rgba.Pix[i+2])
	}
	r, g, b, _ := s.m.At(x, y).RGBA()
	return float64(r >> 8), float64(g >> 8), float64(b >> 8)
}

func luma(r, g, b float64) float64 { return 0.299*r + 0.587*g + 0.114*b }
func cb(r, g, b float64) float64   { return -0.168736*r - 0.331264*g + 0.5*b }
func cr(r, g, b float64) float64   { return 0.5*r - 0.418688*g - 0.081312*b }

// ---- DCT / 量化 ----

var cosTab [8][8]float64

func init() {
	for u := 0; u < 8; u++ {
		c := 1.0
		if u == 0 {
			c = 1 / math.Sqrt2
		}
		for x := 0; x < 8; x++ {
			cosTab[u][x] = c * math.Cos(float64((2*x+1)*u)*math.Pi/16)
		}
	}
}

func fdctQuant(in *[blockSize]float64, q *[blockSize]int32, out *[blockSize]int32) {
	var tmp [blockSize]float64
	for y := 0; y < 8; y++ {
		for u := 0; u < 8; u++ {
			sum := 0.0
			for x := 0; x < 8; x++ {
				sum += in[y*8+x] * cosTab[u][x]
			}
			tmp[y*8+u] = sum
		}
	}
	for v := 0; v < 8; v++ {
		for u := 0; u < 8; u++ {
			sum := 0.0
			for y := 0; y < 8; y++ {
				sum += tmp[y*8+u] * cosTab[v][y]
			}
			val := int32(math.Round(0.25 * sum / float64(q[v*8+u])))
			if val > 1023 {
				val = 1023
			}
			if val < -1023 {
				val = -1023
			}
			out[v*8+u] = val
		}
	}
}

// ---- 熵编码 ----

type bitWriter struct {
	buf *bytes.Buffer
	acc uint64
	n   int
}

func (w *bitWriter) put(bits uint32, n int) {
	if n <= 0 {
		return
	}
	w.acc = (w.acc << uint(n)) | uint64(bits&((1<<uint(n))-1))
	w.n += n
	for w.n >= 8 {
		b := byte(w.acc >> uint(w.n-8))
		w.buf.WriteByte(b)
		if b == 0xFF {
			w.buf.WriteByte(0x00)
		}
		w.n -= 8
	}
}

func (w *bitWriter) flush() {
	if w.n == 0 {
		return
	}
	pad := 8 - w.n%8
	w.put(uint32((1<<uint(pad))-1), pad)
}

func bitLen(v int32) int {
	if v < 0 {
		v = -v
	}
	n := 0
	for v > 0 {
		n++
		v >>= 1
	}
	return n
}

func (w *bitWriter) emitHuff(t *huffTable, sym int) {
	w.put(uint32(t.code[sym]), int(t.size[sym]))
}

func (w *bitWriter) emitValue(v int32, n int) {
	if n == 0 {
		return
	}
	if v < 0 {
		v--
	}
	w.put(uint32(v)&((1<<uint(n))-1), n)
}

func encodeBlock(w *bitWriter, b *[blockSize]int32, prev *int32, dc, ac *huffTable) {
	diff := b[0] - *prev
	*prev = b[0]
	n := bitLen(diff)
	w.emitHuff(dc, n)
	w.emitValue(diff, n)

	run := 0
	for k := 1; k < blockSize; k++ {
		v := b[unzig[k]]
		if v == 0 {
			run++
			continue
		}
		for run > 15 {
			w.emitHuff(ac, 0xF0)
			run -= 16
		}
		n = bitLen(v)
		w.emitHuff(ac, run<<4|n)
		w.emitValue(v, n)
		run = 0
	}
	if run > 0 {
		w.emitHuff(ac, 0x00) // EOB
	}
}

// ---- 标记段 ----

func writeSOI(b *bytes.Buffer) { b.Write([]byte{0xFF, 0xD8}) }

func writeAPP0(b *bytes.Buffer) {
	payload := []byte{'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0}
	writeSegment(b, 0xE0, payload)
}

func writeDQT(b *bytes.Buffer, q [2][blockSize]int32) {
	payload := make([]byte, 0, 2*65)
	for t := 0; t < 2; t++ {
		payload = append(payload, byte(t))
		for i := 0; i < blockSize; i++ {
			payload = append(payload, byte(q[t][i]))
		}
	}
	writeSegment(b, 0xDB, payload)
}

func writeSOF0(b *bytes.Buffer, w, h int) {
	payload := []byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), 3,
		1, 0x22, 0, // Y: 2x2
		2, 0x11, 1, // Cb
		3, 0x11, 1, // Cr
	}
	writeSegment(b, 0xC0, payload)
}

func writeDHT(b *bytes.Buffer, t ...*huffTable) {
	for _, h := range t {
		payload := make([]byte, 0, 1+16+len(h.values))
		payload = append(payload, h.class<<4|h.id)
		payload = append(payload, h.bits[:]...)
		payload = append(payload, h.values...)
		writeSegment(b, 0xC4, payload)
	}
}

func writeDRI(b *bytes.Buffer, interval int) {
	writeSegment(b, 0xDD, []byte{byte(interval >> 8), byte(interval)})
}

func writeSOS(b *bytes.Buffer) {
	payload := []byte{3,
		1, 0x00, // Y: DC0/AC0
		2, 0x11, // Cb: DC1/AC1
		3, 0x11, // Cr
		0, 63, 0,
	}
	writeSegment(b, 0xDA, payload)
}

func writeSegment(b *bytes.Buffer, marker byte, payload []byte) {
	b.WriteByte(0xFF)
	b.WriteByte(marker)
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(payload)+2))
	b.Write(l[:])
	b.Write(payload)
}

// ---- 表 ----

var unzig = [blockSize]int{
	0, 1, 8, 16, 9, 2, 3, 10,
	17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34,
	27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36,
	29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46,
	53, 60, 61, 54, 47, 55, 62, 63,
}

// 标准量化表（zig-zag 顺序），与 Go image/jpeg 的 unscaledQuant 相同。
var unscaledQuant = [2][blockSize]byte{
	{
		16, 11, 12, 14, 12, 10, 16, 14,
		13, 14, 18, 17, 16, 19, 24, 40,
		26, 24, 22, 22, 24, 49, 35, 37,
		29, 40, 58, 51, 61, 60, 57, 51,
		56, 55, 64, 72, 92, 78, 64, 68,
		87, 69, 55, 56, 80, 109, 81, 87,
		95, 98, 103, 104, 103, 62, 77, 113,
		121, 112, 100, 120, 92, 101, 103, 99,
	},
	{
		17, 18, 18, 24, 21, 24, 47, 26,
		26, 47, 99, 66, 56, 66, 99, 99,
		99, 99, 99, 99, 99, 99, 99, 99,
		99, 99, 99, 99, 99, 99, 99, 99,
		99, 99, 99, 99, 99, 99, 99, 99,
		99, 99, 99, 99, 99, 99, 99, 99,
		99, 99, 99, 99, 99, 99, 99, 99,
		99, 99, 99, 99, 99, 99, 99, 99,
	},
}

func quantTables(quality int) (zig [2][blockSize]int32, nat [2][blockSize]int32) {
	scale := 0
	if quality < 50 {
		scale = 5000 / quality
	} else {
		scale = 200 - quality*2
	}
	for t := 0; t < 2; t++ {
		for i := 0; i < blockSize; i++ {
			v := (int(unscaledQuant[t][i])*scale + 50) / 100
			if v < 1 {
				v = 1
			}
			if v > 255 {
				v = 255
			}
			zig[t][i] = int32(v)
			nat[t][unzig[i]] = int32(v)
		}
	}
	return
}

type huffTable struct {
	class  byte
	id     byte
	bits   [16]byte
	values []byte
	code   [256]uint16
	size   [256]uint8
}

func buildTable(class, id byte, bits [16]byte, values []byte) *huffTable {
	t := &huffTable{class: class, id: id, bits: bits, values: values}
	code := 0
	k := 0
	for l := 1; l <= 16; l++ {
		for i := 0; i < int(bits[l-1]); i++ {
			t.code[values[k]] = uint16(code)
			t.size[values[k]] = uint8(l)
			code++
			k++
		}
		code <<= 1
	}
	return t
}

func huffmanTables() (dcLum, acLum, dcChr, acChr *huffTable) {
	dcLum = buildTable(0, 0,
		[16]byte{0, 1, 5, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0},
		[]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11})
	acLum = buildTable(1, 0,
		[16]byte{0, 2, 1, 3, 3, 2, 4, 3, 5, 5, 4, 4, 0, 0, 1, 125},
		[]byte{
			0x01, 0x02, 0x03, 0x00, 0x04, 0x11, 0x05, 0x12,
			0x21, 0x31, 0x41, 0x06, 0x13, 0x51, 0x61, 0x07,
			0x22, 0x71, 0x14, 0x32, 0x81, 0x91, 0xa1, 0x08,
			0x23, 0x42, 0xb1, 0xc1, 0x15, 0x52, 0xd1, 0xf0,
			0x24, 0x33, 0x62, 0x72, 0x82, 0x09, 0x0a, 0x16,
			0x17, 0x18, 0x19, 0x1a, 0x25, 0x26, 0x27, 0x28,
			0x29, 0x2a, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39,
			0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49,
			0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59,
			0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69,
			0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79,
			0x7a, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89,
			0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98,
			0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7,
			0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6,
			0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3, 0xc4, 0xc5,
			0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4,
			0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda, 0xe1, 0xe2,
			0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea,
			0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
			0xf9, 0xfa,
		})
	dcChr = buildTable(0, 1,
		[16]byte{0, 3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0},
		[]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11})
	acChr = buildTable(1, 1,
		[16]byte{0, 2, 1, 2, 4, 4, 3, 4, 7, 5, 4, 4, 0, 1, 2, 119},
		[]byte{
			0x00, 0x01, 0x02, 0x03, 0x11, 0x04, 0x05, 0x21,
			0x31, 0x06, 0x12, 0x41, 0x51, 0x07, 0x61, 0x71,
			0x13, 0x22, 0x32, 0x81, 0x08, 0x14, 0x42, 0x91,
			0xa1, 0xb1, 0xc1, 0x09, 0x23, 0x33, 0x52, 0xf0,
			0x15, 0x62, 0x72, 0xd1, 0x0a, 0x16, 0x24, 0x34,
			0xe1, 0x25, 0xf1, 0x17, 0x18, 0x19, 0x1a, 0x26,
			0x27, 0x28, 0x29, 0x2a, 0x35, 0x36, 0x37, 0x38,
			0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48,
			0x49, 0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58,
			0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68,
			0x69, 0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78,
			0x79, 0x7a, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
			0x88, 0x89, 0x8a, 0x92, 0x93, 0x94, 0x95, 0x96,
			0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5,
			0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4,
			0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3,
			0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2,
			0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda,
			0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9,
			0xea, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
			0xf9, 0xfa,
		})
	return
}
