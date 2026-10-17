package render

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"time"
	"unicode/utf8"
)

// ---- 翻页时钟 ----

func (r *renderer) drawClock(dst *image.RGBA, now time.Time, drawDigits bool) {
	const px, py, pw, ph = 1134, 48, 714, 222
	fillRect(dst, px, py, pw, ph, colClockBG)
	rectOutline(dst, px, py, pw, ph, colFrame)
	cornerMarks(dst, px, py, pw, ph, colAccent)

	digits := fmt.Sprintf("%02d%02d", now.Hour(), now.Minute())
	for i, x := range clockCardX {
		r.drawClockCard(dst, x, clockCardY, clockCardW, clockCardH, digits[i:i+1], drawDigits)
	}

	// 冒号常亮：帧每分钟一换，不做秒级闪烁。
	cx := clockColonX + clockColonGap/2 - 7
	fillRect(dst, cx, 128, 14, 14, colInk)
	fillRect(dst, cx, 176, 14, 14, colInk)
}

// 时钟卡片必须落在 16px 网格上：T13b 的 D 方案按 MCU 整块替换数字。
const (
	clockCardW    = 128
	clockCardH    = 192
	clockCardGap  = 6 // 卡片右缘留出的黑缝，视觉上分隔两张卡
	clockCardY    = 64
	clockColonX   = 1408
	clockColonGap = 128
)

var clockCardX = [4]int{1152, 1280, 1536, 1664}

func (r *renderer) drawClockCard(dst *image.RGBA, x, y, w, h int, digit string, drawDigit bool) {
	inner := w - clockCardGap
	fillRect(dst, x, y, w, h, colClockBG)
	fillRect(dst, x, y, inner, h, colCard)
	if drawDigit {
		r.drawCentered(dst, float64(x)+float64(inner)/2, float64(y), 168, float64(h), -.02,
			colDigit, textRun{digit, fontBarlowBold})
	}
	// 铰链。卡片左右各 2px 的卡扣会伸出卡片/图块，D 方案里省略。
	fillRect(dst, x, y+h/2-2, inner, 4, colHinge)
}

// ---- 主窗口 ----

func (r *renderer) drawMain(dst *image.RGBA, p windowView) {
	const px, py, pw, ph = 72, 290, 1016, 670
	rectOutline(dst, px, py, pw, ph, colFrame)
	cornerMarks(dst, px, py, pw, ph, colAccent)

	const cx, cy, radius, stroke = 422.0, 624.0, 290.0, 36.0
	strokeArc(dst, cx, cy, radius, stroke, 135, 270, colGrid)
	if p.hasU && p.u > 0 {
		strokeArc(dst, cx, cy, radius, stroke, 135, math.Min(p.u, .9999)*270, p.c)
	}
	for _, f := range []float64{0, .25, .5, .75, 1} {
		a := (135 + f*270) * math.Pi / 180
		strokeLine(dst,
			cx+(radius+26)*math.Cos(a), cy+(radius+26)*math.Sin(a),
			cx+(radius+38)*math.Cos(a), cy+(radius+38)*math.Sin(a),
			2, colAccent)
	}
	if p.hasU {
		a := (135 + p.t*270) * math.Pi / 180
		strokeLine(dst,
			cx+(radius-30)*math.Cos(a), cy+(radius-30)*math.Sin(a),
			cx+(radius+30)*math.Cos(a), cy+(radius+30)*math.Sin(a),
			6, colInk)
	}

	r.drawCentered(dst, cx, py+150, 40, 40, 0, colMinor, textRun{p.label, fontNotoMedium})
	drawPercent(dst, r, cx, py+210, 250, 96, 6, p.num, p.unit, p.c)
	r.drawCentered(dst, cx, py+470, 34, 34, 0, colMinor, textRun{p.remT, fontNotoMedium})
	r.drawCentered(dst, cx, py+578, 46, 46, 0, p.c, textRun{p.pred, fontNotoBold})

	const colX, colTop, colW = 802, 354.0, 250
	top := colTop
	for _, st := range stats(p) {
		hline(dst, colX, colX+colW, int(math.Round(top)), colFrame)
		labelTop := top + 1 + 14
		r.drawText(dst, colX, labelTop, 24, 24, 0, colMinor, textRun{st.key, fontNotoMedium})
		r.drawText(dst, colX, labelTop+24+6, 64, 64, 0, st.c, valueRuns(st.value)...)
		top += 1 + 14 + 24 + 6 + 64 + 34
	}
}

// drawPercent 画「大数字 + 小 %」，两者基线对齐、整体水平居中。
func drawPercent(dst *image.RGBA, r *renderer, cx, top, numSize, unitSize, gap float64,
	num, unit string, c color.RGBA) {

	runNum := []textRun{{num, fontBarlowCondensed}}
	wNum := r.measure(runNum, numSize, -.02)
	wUnit := 0.0
	if unit != "" {
		wUnit = r.measure([]textRun{{unit, fontBarlowCondensed}}, unitSize, 0)
	}
	total := wNum + gap + wUnit
	x := cx - total/2
	baseline := r.baseline(fontBarlowCondensed, numSize, numSize, top)
	r.drawText(dst, x, top, numSize, numSize, -.02, c, runNum...)
	if unit != "" {
		unitTop := baseline - r.ascent(fontBarlowCondensed, unitSize)
		r.drawText(dst, x+wNum+gap, unitTop, unitSize, unitSize, 0, c,
			textRun{unit, fontBarlowCondensed})
	}
}

// ---- 次窗口 ----

func (r *renderer) drawSecondary(dst *image.RGBA, p windowView) {
	const px, py, pw, ph = 1160, 290, 688, 380
	rectOutline(dst, px, py, pw, ph, colFrame)
	cornerMarks(dst, px, py, pw, ph, colAccent)

	const titleTop = 326.0
	base := r.baseline(fontNotoMedium, 36, 36, titleTop)
	r.drawText(dst, 1200, titleTop, 36, 36, 0, colMinor, textRun{p.label, fontNotoMedium})
	remTop := base - r.ascent(fontNotoMedium, 28)
	r.drawRight(dst, 1808, remTop, 28, 28, 0, colMinor, textRun{p.remT, fontNotoMedium})

	const cx, cy, radius, stroke = 1310.0, 520.0, 96.0, 20.0
	strokeArc(dst, cx, cy, radius, stroke, 135, 270, colGrid)
	if p.hasU && p.u > 0 {
		strokeArc(dst, cx, cy, radius, stroke, 135, math.Min(p.u, .9999)*270, p.c)
	}
	if p.hasU {
		a := (135 + p.t*270) * math.Pi / 180
		strokeLine(dst,
			cx+(radius-18)*math.Cos(a), cy+(radius-18)*math.Sin(a),
			cx+(radius+18)*math.Cos(a), cy+(radius+18)*math.Sin(a),
			4, colInk)
	}

	// 数字左对齐在 (310,118)，基线对齐
	const numLeft, numTop = 1470.0, 408.0
	wNum := r.measure([]textRun{{p.num, fontBarlowCondensed}}, 150, 0)
	base2 := r.baseline(fontBarlowCondensed, 150, 150, numTop)
	r.drawText(dst, numLeft, numTop, 150, 150, 0, p.c, textRun{p.num, fontBarlowCondensed})
	if p.unit != "" {
		unitTop := base2 - r.ascent(fontBarlowCondensed, 60)
		r.drawText(dst, numLeft+wNum+4, unitTop, 60, 60, 0, p.c,
			textRun{p.unit, fontBarlowCondensed})
	}
	r.drawText(dst, 1474, 576, 34, 34, 0, p.c, textRun{p.pred, fontNotoBold})
}

// ---- 「怎么读」 ----

func (r *renderer) drawLegend(dst *image.RGBA) {
	const px, py, pw, ph = 1160, 710, 688, 250
	rectOutline(dst, px, py, pw, ph, colFrame)
	cornerMarks(dst, px, py, pw, ph, colAccent)

	r.drawText(dst, 1200, 744, 26, 26, .12, colMinor, textRun{"怎么读", fontNotoMedium})

	// 示意弧：SVG (30,84) 偏移 → 画布 (1190,794)
	strokeArc(dst, 1290, 914, 100, 18, 180, 180, colGrid)
	strokeArc(dst, 1290, 914, 100, 18, 180, 90, colWarn)
	strokeLine(dst, 1275, 826, 1266, 798, 4, colInk)

	const textX = 1464.0
	rows := []struct {
		block  bool
		bw, bh float64
		c      color.RGBA
		text   string
		color  color.RGBA
	}{
		{true, 28, 12, colWarn, "彩色弧 · 已用额度", colInk},
		{true, 28, 4, colInk, "白色刻度 · 时间进度", colInk},
		{false, 0, 0, colInk, "弧越过刻度 = 用得比时间快", colMinor},
	}
	top := 796.0
	const lineH = 28 * 1.1
	for _, row := range rows {
		x := 1420.0
		if row.block {
			fillRect(dst, 1420, int(math.Round(top+(lineH-row.bh)/2)), int(row.bw), int(row.bh), row.c)
			x = textX
		}
		r.drawText(dst, x, top, 28, lineH, 0, row.color, textRun{row.text, fontNotoMedium})
		top += lineH + 18
	}
}

// valueRuns 把统计值按 ASCII / 中文切段：数字和 % 用 Barlow Condensed，
// 中文（「快」「/天」里的「天」）回退到 Noto Sans SC。
func valueRuns(s string) []textRun {
	return splitRuns(s, fontBarlowCondensed, fontNotoMedium)
}

// splitRuns 按 ASCII / 非 ASCII 切段，分别用两种字体。
func splitRuns(s string, ascii, cjk fontKind) []textRun {
	if s == "" {
		return nil
	}
	var out []textRun
	var cur []rune
	curASCII := true
	flush := func() {
		if len(cur) == 0 {
			return
		}
		k := cjk
		if curASCII {
			k = ascii
		}
		out = append(out, textRun{string(cur), k})
		cur = cur[:0]
	}
	for len(s) > 0 {
		rr, n := utf8.DecodeRuneInString(s)
		ascii := rr < utf8.RuneSelf
		if len(cur) > 0 && ascii != curASCII {
			flush()
		}
		curASCII = ascii
		cur = append(cur, rr)
		s = s[n:]
	}
	flush()
	return out
}

// ClockSlot 是一张数字卡片在画布上的位置，以及它起始的 MCU 列。
type ClockSlot struct {
	X, Y, W, H int
	MCUCol     int
}

// ClockSlots 对 T13b 的 D 方案公开四张卡片的位置。卡片宽 8 个 MCU、
// 高 12 个 MCU，起始列都是 8 的整数倍（DRI=8 的 restart 边界）。
var ClockSlots = [4]ClockSlot{
	{1152, 64, 128, 192, 72},
	{1280, 64, 128, 192, 80},
	{1536, 64, 128, 192, 96},
	{1664, 64, 128, 192, 104},
}

// 时钟带：第 4..15 MCU 行（y 64..256），每条 restart 区间 8 个 MCU。
const (
	ClockBandRow0 = 4
	ClockBandRow1 = 16
	ClockMCUCols  = 120
	ClockDRI      = 8
	ClockCardRows = 12
)
