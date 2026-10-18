package render

import (
	"image/color"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/source"
)

// 达成型窗口与消耗型共用环的几何和配速数学，但配色/排序/文案全部相反。
// 这组用例把「超前是好事」钉住：同样的 u 和 t，两个方向要给出相反的结论。

var dirNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// 一个当日窗口：00:00 开始、次日 00:00 重置，now 正好过半。
func dayWindow(label string, u float64, dir source.Direction) source.Window {
	return source.Window{
		Label:          label,
		Length:         24 * time.Hour,
		UsedPercentage: u,
		ResetsAt:       dirNow.Add(12 * time.Hour),
		Dir:            dir,
	}
}

func TestTargetWindowPolarity(t *testing.T) {
	cases := []struct {
		name string
		u    float64
		c    color.RGBA
		rank int
		done bool
		pred string
		head string
	}{
		// 时间过半、达成 105%：业绩，绿色，且让出主位。
		{"已达成", 1.05, colSafe, 1, true, "已达成", "今日成交额已达成"},
		// 时间过半、达成 60%：按配速 8 小时后就能到 100%，赶得上。
		{"赶得上", .6, colSafe, 1, false, "约8时00分达成", "两个目标都在节奏上"},
		// 时间过半、只做到 30%：按配速收盘时 60%，落后但不算崩。
		{"偏慢", .3, colWarn, 2, false, "预计仅 60%", "今日成交额进度偏慢"},
		// 时间过半、只做到 10%：收盘预计 20%，严重落后。
		{"严重落后", .1, colBad, 4, false, "预计仅 20%", "今日成交额进度严重落后"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := dayWindow("今日成交额", tc.u, source.Target)
			got := evalWindow(&w, dirNow, "窗口", source.Target)
			if got.c != tc.c {
				t.Errorf("颜色 = %v，要 %v", got.c, tc.c)
			}
			if got.rank != tc.rank {
				t.Errorf("rank = %d，要 %d", got.rank, tc.rank)
			}
			if got.done != tc.done {
				t.Errorf("done = %v，要 %v", got.done, tc.done)
			}
			if got.pred != tc.pred {
				t.Errorf("预测 = %q，要 %q", got.pred, tc.pred)
			}
			if h, _ := headline(got); h != tc.head {
				t.Errorf("结论句 = %q，要 %q", h, tc.head)
			}
		})
	}
}

// 同一组数字换成消耗型必须得出相反结论：达成 60% 的「赶得上」在额度语义下
// 是「8 小时后就见底」的告警。这一条是方向概念存在的全部理由。
func TestSameNumbersOppositeVerdict(t *testing.T) {
	wt := dayWindow("今日成交额", .6, source.Target)
	wb := dayWindow("日额度", .6, source.Budget)
	vt := evalWindow(&wt, dirNow, "窗口", source.Target)
	vb := evalWindow(&wb, dirNow, "窗口", source.Budget)

	if vt.u != vb.u || vt.t != vb.t || vt.pace != vb.pace {
		t.Fatalf("两个方向的配速数学应当一致: %+v vs %+v", vt, vb)
	}
	if vt.c != colSafe {
		t.Errorf("达成型超前应为安全色，得到 %v", vt.c)
	}
	if vb.c != colWarn {
		t.Errorf("消耗型超前应为告警色，得到 %v", vb.c)
	}
	if vt.pred != "约8时00分达成" || vb.pred != "约8时00分用完" {
		t.Errorf("预测措辞应相反: %q / %q", vt.pred, vb.pred)
	}
	// 剩 40% 要在剩下的半天里跑完，折算成日速度就是 80%/天。
	if vt.budget != "还需 80.0%/天" || vb.budget != "80.0%/天" {
		t.Errorf("速度建议措辞应相反: %q / %q", vt.budget, vb.budget)
	}
}

// 两个目标都在节奏上时，主位要给跑得慢的那个（最需要被催的）；
// 消耗型相反，主位给跑得快的（最快见底的）。
func TestTargetTieBreakPicksLaggard(t *testing.T) {
	snap := source.Snapshot{
		Now: dirNow,
		Windows: []source.Window{
			dayWindow("今日订单", .9, source.Target),
			dayWindow("今日成交额", .7, source.Target),
		},
	}
	// Length 相同，排序稳定，两个 rank 都是 1，只能靠配速定主次。
	snap.Windows[1].Length = 24*time.Hour + time.Second // 让长窗口确定是成交额
	main, sec := views(snap)
	if main.label != "今日成交额" {
		t.Errorf("主位 = %q，要落后的「今日成交额」", main.label)
	}
	if sec.label != "今日订单" {
		t.Errorf("次位 = %q，要「今日订单」", sec.label)
	}
}
