package raster

import "encoding/binary"

// 适配测试用的校准页。
//
// **这是唯一由服务端生成光栅的地方，而且是刻意的。**
//
// 「服务端不渲染」那条约束针对的是用户文档：没有 CUPS、没有字体、没有 PPD，
// 而文档在手机上。校准页一样都不沾——固定图案、没有字体、没有用户内容，
// 而且服务端恰好最清楚该用什么像素尺寸（它就是 render-profile 的出处）。
// 让 App 生成的话，尺寸要跨一次网络往返才对得上，还要求两个平台的原生
// 光栅器都就绪。
//
// 字节布局与 app/shared/urf 那份 C++ 编码器完全一致，依据是 Apple 光栅器的
// 真实产物（app/shared/urf/tests/fixtures/）：
//
//	文件头 12 字节：UNIRAST\0 + 大端页数
//	每页 32 字节页头：bpp / colorspace / duplex / quality / 保留 / 宽 / 高 / dpi / 保留
//	每行：[1 字节重复计数 N][RLE 包...]，像素数攒够页宽即结束，**没有终止符**
//	  n<128   → 接 1 个像素，重复 n+1 次
//	  n>128   → 接 257-n 个字面像素
//	  n=128   → 不是合法值

const (
	urfBPP       = 8
	urfColorGray = 0 // 已用 Apple 光栅器的产物核对
	urfDuplexOne = 1 // URF 的 duplex 枚举里 1 = 单面，0 不是合法值
	urfMaxRun    = 128
	white        = 0xFF
	black        = 0x00
)

// encodeRow 把一行 8 位灰度像素编成 URF 的 RLE。不含行重复计数，也没有终止符。
func encodeRow(row []byte, out []byte) []byte {
	i := 0
	for i < len(row) {
		run := 1
		for i+run < len(row) && row[i+run] == row[i] && run < urfMaxRun {
			run++
		}
		if run >= 2 {
			out = append(out, byte(run-1), row[i])
			i += run
			continue
		}
		start := i
		for i < len(row) && i-start < urfMaxRun {
			r := 1
			for i+r < len(row) && row[i+r] == row[i] {
				r++
			}
			if r >= 2 {
				break
			}
			i++
		}
		if n := i - start; n == 1 {
			// 257-1 = 256 会溢出。单个像素退回游程编码。
			out = append(out, 0, row[start])
		} else {
			out = append(out, byte(257-n))
			out = append(out, row[start:i]...)
		}
	}
	return out
}

// TestPageKind 决定画什么图案。
type TestPageKind string

const (
	// KindMark 是一条粗横带。够醒目、够省字节，用来数「出了几张纸」。
	KindMark TestPageKind = "mark"
	// KindMargins 是贴边的方框，用来量四边被切掉多少。
	KindMargins TestPageKind = "margins"
)

// TestPage 生成一页 URF 校准页。
func TestPage(widthPx, heightPx, dpi int, kind TestPageKind) []byte {
	if widthPx <= 0 || heightPx <= 0 {
		return nil
	}
	out := make([]byte, 0, 8192)

	out = append(out, 'U', 'N', 'I', 'R', 'A', 'S', 'T', 0)
	out = binary.BigEndian.AppendUint32(out, 1) // 页数。写 0 打印机认为文档为空
	out = append(out, urfBPP, urfColorGray, urfDuplexOne, 0)
	out = append(out, make([]byte, 8)...)
	out = binary.BigEndian.AppendUint32(out, uint32(widthPx))
	out = binary.BigEndian.AppendUint32(out, uint32(heightPx))
	out = binary.BigEndian.AppendUint32(out, uint32(dpi))
	out = append(out, make([]byte, 8)...)

	row := make([]byte, widthPx)
	// 线宽取 dpi/12（600dpi 下 50px ≈ 2mm），至少 4px，肉眼看得见也量得准。
	thick := dpi / 12
	if thick < 4 {
		thick = 4
	}

	// 逐行生成，相同的连续行用重复计数合并——空白页因此只有几百字节。
	var prev []byte
	repeat := 0
	flush := func() {
		if repeat > 0 {
			out = append(out, byte(repeat-1))
			out = append(out, prev...)
			repeat = 0
		}
	}
	for y := 0; y < heightPx; y++ {
		fillRow(row, y, widthPx, heightPx, thick, kind)
		enc := encodeRow(row, nil)
		// 重复计数只有 1 字节，最多 256 行
		if repeat > 0 && repeat < 256 && equal(enc, prev) {
			repeat++
			continue
		}
		flush()
		prev = enc
		repeat = 1
	}
	flush()
	return out
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fillRow(row []byte, y, w, h, thick int, kind TestPageKind) {
	for i := range row {
		row[i] = white
	}
	switch kind {
	case KindMargins:
		// 贴边方框：上下各一条满宽的带，其余行两侧各一条。
		// 被切掉多少，量剩下的边框到纸边的距离就知道。
		if y < thick || y >= h-thick {
			for i := range row {
				row[i] = black
			}
			return
		}
		for i := 0; i < thick && i < w; i++ {
			row[i] = black
			row[w-1-i] = black
		}
	default: // KindMark
		// 页面上部三分之一处一条粗横带。数纸的时候一眼能认出这是测试页。
		top := h / 3
		if y >= top && y < top+thick*4 {
			lo, hi := w/6, w-w/6
			for i := lo; i < hi; i++ {
				row[i] = black
			}
		}
	}
}
