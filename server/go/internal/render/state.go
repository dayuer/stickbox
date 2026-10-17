package render

import (
	"fmt"
	"image/color"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/source"
)

// 设计稿的调色板。render 内部一律用 RGBA，alpha 恒为 255。
var (
	colBG      = color.RGBA{0x1D, 0x2D, 0x3D, 0xFF}
	colGrid    = color.RGBA{0x2C, 0x45, 0x5D, 0xFF}
	colFrame   = color.RGBA{0x41, 0x61, 0x80, 0xFF}
	colAccent  = color.RGBA{0x74, 0x9D, 0xC4, 0xFF}
	colMinor   = color.RGBA{0x94, 0xBC, 0xE3, 0xFF}
	colInk     = color.RGBA{0xF5, 0xF5, 0xF8, 0xFF}
	colSafe    = color.RGBA{0x3D, 0xDC, 0x84, 0xFF}
	colWarn    = color.RGBA{0xFF, 0xB0, 0x20, 0xFF}
	colBad     = color.RGBA{0xFF, 0x5A, 0x5A, 0xFF}
	colOff     = color.RGBA{0x7A, 0x7A, 0x7D, 0xFF}
	colCard    = color.RGBA{0xF5, 0xF5, 0xF8, 0xFF}
	colDigit   = color.RGBA{0x1D, 0x1F, 0x20, 0xFF}
	colHinge   = color.RGBA{0x2B, 0x2B, 0x2D, 0xFF}
	colClockBG = color.RGBA{0x00, 0x00, 0x00, 0xFF}
)

// windowView 是一个窗口在帧里的全部派生值，与设计稿 renderVals().win()
// 的返回结构一一对应。
type windowView struct {
	label  string
	none   bool
	rank   int
	pace   float64
	u, t   float64
	hasU   bool
	c      color.RGBA
	num    string
	unit   string
	remT   string
	pred   string
	budget string
}

// views 把快照拆成短窗口 / 长窗口两份派生状态，并完成主次分配。
func views(s source.Snapshot) (main, secondary windowView) {
	wins := append([]source.Window(nil), s.Windows...)
	sort.SliceStable(wins, func(i, j int) bool { return wins[i].Length < wins[j].Length })

	var short, long *source.Window
	if len(wins) > 0 {
		short = &wins[0]
	}
	if len(wins) > 1 {
		long = &wins[1]
	}
	w5 := evalWindow(short, s.Now, "5 小时")
	w7 := evalWindow(long, s.Now, "7 天")

	if w5.rank != w7.rank {
		if w5.rank > w7.rank {
			return w5, w7
		}
		return w7, w5
	}
	if w5.pace >= w7.pace {
		return w5, w7
	}
	return w7, w5
}

func evalWindow(w *source.Window, now time.Time, fallbackLabel string) windowView {
	if w == nil || w.Length <= 0 {
		return windowView{
			label: fallbackLabel, none: true, c: colOff, num: "--", pred: "等待数据",
		}
	}
	label := w.Label
	if label == "" {
		label = "窗口"
	}
	u := w.UsedPercentage
	rem := w.ResetsAt.Sub(now).Seconds()
	if rem < 0 {
		rem = 0
	}
	W := w.Length.Seconds()
	t := 1 - rem/W

	if rem <= 0 {
		return windowView{
			label: label, none: true, c: colOff, num: "--", remT: "已重置", pred: "等新数据",
		}
	}

	toFull := math.Inf(1)
	elapsed := W - rem
	if u > 0 && elapsed > 0 {
		toFull = (1 - u) / (u / elapsed)
	}
	c, rank, pred := colSafe, 1, "够用到重置"
	switch {
	case u >= 1:
		c, rank, pred = colBad, 4, "已用满"
	case t >= .05 && toFull < rem:
		if u >= .9 {
			c, rank = colBad, 3
		} else {
			c, rank = colWarn, 2
		}
		pred = "约" + fmtDur(toFull, false) + "用完"
	}

	unitName, perSec := "天", 86400.0
	if w.Length < 24*time.Hour {
		unitName, perSec = "时", 3600.0
	}
	budget := "--"
	if u < 1 {
		budget = fmt.Sprintf("%.1f%%/%s", (1-u)/(rem/perSec)*100, unitName)
	}
	return windowView{
		label: label, rank: rank, pace: u / math.Max(t, .01),
		u: u, t: t, hasU: true, c: c,
		num: strconv.Itoa(int(math.Round(u * 100))), unit: "%",
		remT:   "剩 " + fmtDur(rem, rem < 86400),
		pred:   pred,
		budget: budget,
	}
}

// fmtDur 与设计稿的 fmt 完全一致：
//
//	>=1 天：X天Y时（withS 时再带 MM分）
//	>=1 小时：X时MM分（withS 时再带 SS秒）
//	否则：M分（withS 时再带 SS秒）
func fmtDur(sec float64, withS bool) string {
	if sec < 0 {
		sec = 0
	}
	total := int(math.Floor(sec))
	d := total / 86400
	hh := total % 86400 / 3600
	m := total % 3600 / 60
	ss := total % 60
	two := func(n int) string { return fmt.Sprintf("%02d", n) }
	switch {
	case d > 0:
		out := fmt.Sprintf("%d天%d时", d, hh)
		if withS {
			out += two(m) + "分"
		}
		return out
	case hh > 0:
		out := fmt.Sprintf("%d时%s分", hh, two(m))
		if withS {
			out += two(ss) + "秒"
		}
		return out
	default:
		out := fmt.Sprintf("%d分", m)
		if withS {
			out += two(ss) + "秒"
		}
		return out
	}
}

// headline 返回结论句与颜色。
func headline(p windowView) (string, color.RGBA) {
	switch {
	case p.none:
		return "等待电脑上报数据", colInk
	case p.rank == 4:
		return p.label + "额度已用满", p.c
	case p.rank == 3:
		return p.label + "额度快用完了", p.c
	case p.rank == 2:
		return p.label + "额度用得偏快", p.c
	default:
		return "两个窗口都够用", colInk
	}
}

type statView struct {
	key, value string
	c          color.RGBA
}

func stats(p windowView) []statView {
	if p.none {
		return []statView{
			{"已用", "--", colOff},
			{"时间进度", "--", colOff},
			{"节奏", "--", colOff},
			{"建议上限", "--", colOff},
		}
	}
	diff := int(math.Round((p.u - p.t) * 100))
	pace, paceC := "慢 "+strconv.Itoa(-diff)+"%", colSafe
	if diff > 0 {
		pace, paceC = "快 "+strconv.Itoa(diff)+"%", p.c
	}
	return []statView{
		{"已用", strconv.Itoa(int(math.Round(p.u*100))) + "%", colInk},
		{"时间进度", strconv.Itoa(int(math.Round(p.t*100))) + "%", colInk},
		{"节奏", pace, paceC},
		{"建议上限", p.budget, colInk},
	}
}
