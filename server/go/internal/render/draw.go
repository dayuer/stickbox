package render

import (
	"image"
	"image/color"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// blend 把 c 以 coverage 画到 dst 上（source-over，dst 不透明）。
func blend(dst *image.RGBA, x, y int, c color.RGBA, coverage float64) {
	if x < 0 || y < 0 || x >= dst.Rect.Dx() || y >= dst.Rect.Dy() {
		return
	}
	a := clamp01(coverage)
	if a == 0 {
		return
	}
	i := dst.PixOffset(x, y)
	pa := float64(dst.Pix[i+3]) / 255
	oa := a + pa*(1-a)
	if oa == 0 {
		return
	}
	src := [3]float64{float64(c.R), float64(c.G), float64(c.B)}
	for k := 0; k < 3; k++ {
		old := float64(dst.Pix[i+k])
		dst.Pix[i+k] = uint8(math.Round((src[k]*a + old*pa*(1-a)) / oa))
	}
	dst.Pix[i+3] = uint8(math.Round(oa * 255))
}

func fillRect(dst *image.RGBA, x, y, w, h int, c color.RGBA) {
	r := image.Rect(x, y, x+w, y+h).Intersect(dst.Rect)
	for yy := r.Min.Y; yy < r.Max.Y; yy++ {
		i := dst.PixOffset(r.Min.X, yy)
		for xx := r.Min.X; xx < r.Max.X; xx++ {
			dst.Pix[i] = c.R
			dst.Pix[i+1] = c.G
			dst.Pix[i+2] = c.B
			dst.Pix[i+3] = c.A
			i += 4
		}
	}
}

func hline(dst *image.RGBA, x0, x1, y int, c color.RGBA) {
	if y < 0 || y >= dst.Rect.Dy() {
		return
	}
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	if x0 < 0 {
		x0 = 0
	}
	if x1 > dst.Rect.Dx() {
		x1 = dst.Rect.Dx()
	}
	for x := x0; x < x1; x++ {
		i := dst.PixOffset(x, y)
		dst.Pix[i] = c.R
		dst.Pix[i+1] = c.G
		dst.Pix[i+2] = c.B
		dst.Pix[i+3] = c.A
	}
}

func vline(dst *image.RGBA, x, y0, y1 int, c color.RGBA) {
	if x < 0 || x >= dst.Rect.Dx() {
		return
	}
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	if y0 < 0 {
		y0 = 0
	}
	if y1 > dst.Rect.Dy() {
		y1 = dst.Rect.Dy()
	}
	for y := y0; y < y1; y++ {
		i := dst.PixOffset(x, y)
		dst.Pix[i] = c.R
		dst.Pix[i+1] = c.G
		dst.Pix[i+2] = c.B
		dst.Pix[i+3] = c.A
	}
}

func rectOutline(dst *image.RGBA, x, y, w, h int, c color.RGBA) {
	hline(dst, x, x+w, y, c)
	hline(dst, x, x+w, y+h-1, c)
	vline(dst, x, y, y+h, c)
	vline(dst, x+w-1, y, y+h, c)
}

// cornerMarks 画设计稿的 15×15「+」角标，中心正好落在矩形角点上。
func cornerMarks(dst *image.RGBA, x, y, w, h int, c color.RGBA) {
	for _, p := range [][2]int{{x, y}, {x + w, y}, {x, y + h}, {x + w, y + h}} {
		hline(dst, p[0]-7, p[0]+8, p[1], c)
		vline(dst, p[0], p[1]-7, p[1]+8, c)
	}
}

// strokeLine 画一条有 1px 抗锯齿边缘的粗线（butt 端）。
func strokeLine(dst *image.RGBA, x0, y0, x1, y1, width float64, c color.RGBA) {
	dx, dy := x1-x0, y1-y0
	length2 := dx*dx + dy*dy
	minX := int(math.Floor(math.Min(x0, x1) - width))
	maxX := int(math.Ceil(math.Max(x0, x1) + width))
	minY := int(math.Floor(math.Min(y0, y1) - width))
	maxY := int(math.Ceil(math.Max(y0, y1) + width))
	half := width / 2
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			px, py := float64(x)+.5, float64(y)+.5
			var t float64
			if length2 > 0 {
				t = ((px-x0)*dx + (py-y0)*dy) / length2
			}
			t = clamp01(t)
			cx, cy := x0+t*dx, y0+t*dy
			d := math.Hypot(px-cx, py-cy)
			cov := clamp01(half + .5 - d)
			if cov > 0 {
				blend(dst, x, y, c, cov)
			}
		}
	}
}

// strokeArc 画一段圆弧。角度单位是度，0° 指向右，y 向下，正方向为顺时针。
// start/sweep 与设计稿的 SVG path 一致（135° 起、270° 扫过）。
func strokeArc(dst *image.RGBA, cx, cy, r, width, startDeg, sweepDeg float64, c color.RGBA) {
	if sweepDeg <= 0 {
		return
	}
	half := width / 2
	minX := int(math.Floor(cx - r - half - 1))
	maxX := int(math.Ceil(cx + r + half + 1))
	minY := int(math.Floor(cy - r - half - 1))
	maxY := int(math.Ceil(cy + r + half + 1))
	sweepRad := sweepDeg * math.Pi / 180
	arcLen := r * sweepRad
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			px, py := float64(x)+.5, float64(y)+.5
			dx, dy := px-cx, py-cy
			d := math.Abs(math.Hypot(dx, dy) - r)
			cov := clamp01(half + .5 - d)
			if cov == 0 {
				continue
			}
			ang := math.Atan2(dy, dx) * 180 / math.Pi
			delta := math.Mod(ang-startDeg+720, 360)
			// 端点沿弧长方向留 0.5px 抗锯齿
			along := delta * math.Pi / 180 * r
			cov *= clamp01(math.Min(along, arcLen-along) + .5)
			if cov > 0 {
				blend(dst, x, y, c, cov)
			}
		}
	}
}

// ---- 文字 ----

type fontKind int

const (
	fontBarlowCondensed fontKind = iota
	fontBarlowBold
	fontNotoMedium
	fontNotoBold
)

type textRun struct {
	text string
	font fontKind
}

type renderer struct {
	fonts map[fontKind]*opentype.Font
}

// baseline 按 CSS 行盒规则算基线：内容高度 ascent+descent，
// 半行距把内容盒在 line-height 内居中。
func (r *renderer) baseline(k fontKind, size, lineHeight, top float64) float64 {
	f, err := r.face(k, size)
	if err != nil {
		return top + size*.8
	}
	defer f.Close()
	m := f.Metrics()
	ascent := float64(m.Ascent) / 64
	descent := float64(m.Descent) / 64
	return top + (lineHeight-(ascent+descent))/2 + ascent
}

func (r *renderer) face(k fontKind, size float64) (font.Face, error) {
	return opentype.NewFace(r.fonts[k], &opentype.FaceOptions{
		Size: size, DPI: 72, Hinting: font.HintingNone,
	})
}

// measure 返回一段混排文字的宽度（px）。tracking 是 em 比例（CSS letter-spacing）。
func (r *renderer) measure(runs []textRun, size, tracking float64) float64 {
	var w float64
	for _, run := range runs {
		f, err := r.face(run.font, size)
		if err != nil {
			continue
		}
		if tracking == 0 {
			w += float64(font.MeasureString(f, run.text)) / 64
		} else {
			for _, ch := range run.text {
				w += float64(font.MeasureString(f, string(ch))) / 64
				w += tracking * size
			}
		}
		f.Close()
	}
	return w
}

// drawText 在 top 处画一行文字，返回结束 x。
// lineHeight 为 CSS line-height；文字基线按半行距 + ascent 计算。
func (r *renderer) drawText(dst *image.RGBA, x, top, size, lineHeight, tracking float64,
	c color.RGBA, runs ...textRun) float64 {

	if len(runs) == 0 {
		return x
	}
	baseline := r.baseline(runs[0].font, size, lineHeight, top)

	for _, run := range runs {
		f, err := r.face(run.font, size)
		if err != nil {
			continue
		}
		d := &font.Drawer{
			Dst:  dst,
			Src:  image.NewUniform(c),
			Face: f,
			Dot:  fixed.P(int(math.Round(x)), int(math.Round(baseline))),
		}
		if tracking == 0 {
			d.DrawString(run.text)
			x += float64(font.MeasureString(f, run.text)) / 64
		} else {
			for _, ch := range run.text {
				d.Dot = fixed.P(int(math.Round(x)), int(math.Round(baseline)))
				d.DrawString(string(ch))
				x += float64(font.MeasureString(f, string(ch))) / 64
				x += tracking * size
			}
		}
		f.Close()
	}
	return x
}

func (r *renderer) drawCentered(dst *image.RGBA, cx, top, size, lineHeight, tracking float64,
	c color.RGBA, runs ...textRun) {

	w := r.measure(runs, size, tracking)
	r.drawText(dst, cx-w/2, top, size, lineHeight, tracking, c, runs...)
}

func (r *renderer) drawRight(dst *image.RGBA, right, top, size, lineHeight, tracking float64,
	c color.RGBA, runs ...textRun) {

	w := r.measure(runs, size, tracking)
	r.drawText(dst, right-w, top, size, lineHeight, tracking, c, runs...)
}
