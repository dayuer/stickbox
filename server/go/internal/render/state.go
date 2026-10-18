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
	label string
	none  bool
	// rank 是「该上主位的程度」1..4，两个方向同义：数字越大越该被看见。
	// 消耗型是「越接近用满越该看」，达成型是「越落后越该看」。
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
	// dir/done 只影响文案与配色方向：达成型把「超前」当好事。
	dir  source.Direction
	done bool
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
	// 窗口缺位时的占位名随方向走：额度看板缺的是「5 小时 / 7 天」，
	// 经营看板缺的是某个指标，不能把 Claude 的窗口名漏到别的数据源上。
	dir := s.Dir
	if short != nil {
		dir = short.Dir
	}
	f5, f7 := "5 小时", "7 天"
	if dir == source.Target {
		f5, f7 = "今日指标", "今日指标"
	}
	w5 := evalWindow(short, s.Now, f5, dir)
	w7 := evalWindow(long, s.Now, f7, dir)

	if w5.rank != w7.rank {
		if w5.rank > w7.rank {
			return w5, w7
		}
		return w7, w5
	}
	// 两路数据各占一环时方向会混合，这时配速不可比：一个是「花得多快」，
	// 一个是「赚得多快」。同级就让时间尺度短的上主位，它的窗口更紧。
	if w5.dir != w7.dir {
		return w5, w7
	}
	// 同级时看配速，但方向相反：消耗型挑跑得最快的（最快见底），
	// 达成型挑跑得最慢的（最需要被催）。
	ahead := w5.pace >= w7.pace
	if w5.dir == source.Target {
		ahead = w5.pace <= w7.pace
	}
	if ahead {
		return w5, w7
	}
	return w7, w5
}

func evalWindow(w *source.Window, now time.Time, fallbackLabel string, dir source.Direction) windowView {
	if w == nil || w.Length <= 0 {
		return windowView{
			label: fallbackLabel, none: true, c: colOff, num: "--", pred: "等待数据", dir: dir,
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
			dir: w.Dir,
		}
	}

	// toFull 是「按当前配速还要多久走到 100%」。两个方向用的是同一个数：
	// 消耗型希望它比剩余时间长（撑得住），达成型希望它比剩余时间短（赶得上）。
	toFull := math.Inf(1)
	elapsed := W - rem
	if u > 0 && elapsed > 0 {
		toFull = (1 - u) / (u / elapsed)
	}
	// projected 是按当前配速到窗口结束时的最终比例，只有达成型用得上。
	projected := u / math.Max(t, .01)

	unitName, perSec := "天", 86400.0
	if w.Length < 24*time.Hour {
		unitName, perSec = "时", 3600.0
	}
	// rate 是剩余时间里的「每单位应有速度」：消耗型是还能花多少，
	// 达成型是还得跑多少。公式相同，只有措辞相反。
	rate := fmt.Sprintf("%.1f%%/%s", (1-u)/(rem/perSec)*100, unitName)

	c, rank, done := colSafe, 1, false
	var pred, budget string
	if w.Dir == source.Target {
		pred, budget = "配速达标", "还需 "+rate
		switch {
		case u >= 1:
			// 达成是业绩不是故障：留在主位最低优先级，让落后的那个顶上来。
			c, rank, done, pred, budget = colSafe, 1, true, "已达成", "已达成"
		case t >= .05 && toFull > rem:
			if projected < .5 {
				c, rank = colBad, 4
			} else {
				c, rank = colWarn, 2
			}
			pred = fmt.Sprintf("预计仅 %d%%", int(math.Round(projected*100)))
		default:
			pred = "约" + fmtDur(toFull, false) + "达成"
		}
	} else {
		pred, budget = "够用到重置", rate
		switch {
		case u >= 1:
			c, rank, pred, budget = colBad, 4, "已用满", "--"
		case t >= .05 && toFull < rem:
			if u >= .9 {
				c, rank = colBad, 3
			} else {
				c, rank = colWarn, 2
			}
			pred = "约" + fmtDur(toFull, false) + "用完"
		}
	}
	return windowView{
		label: label, rank: rank, pace: projected,
		u: u, t: t, hasU: true, c: c,
		num: strconv.Itoa(int(math.Round(u * 100))), unit: "%",
		remT:   "剩 " + fmtDur(rem, rem < 86400),
		pred:   pred,
		budget: budget,
		dir:    w.Dir, done: done,
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

// headline 返回结论句与颜色。措辞按窗口方向分叉：额度见底是故障，
// 目标达成是业绩，两者的 rank 同义但结论句相反。
func headline(p windowView) (string, color.RGBA) {
	if p.dir == source.Target {
		switch {
		case p.none:
			return "等待数据上报", colInk
		case p.done:
			return p.label + "已达成", p.c
		case p.rank == 4:
			return p.label + "进度严重落后", p.c
		case p.rank == 2:
			return p.label + "进度偏慢", p.c
		default:
			return "两个目标都在节奏上", colInk
		}
	}
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
