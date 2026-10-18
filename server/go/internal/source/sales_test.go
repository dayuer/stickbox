package source

import (
	"context"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/sales"
)

type fakeSalesStore struct {
	rep      sales.Report
	received time.Time
	ok       bool
}

func (f fakeSalesStore) Latest(string) (sales.Report, time.Time, bool, error) {
	return f.rep, f.received, f.ok, nil
}

var shanghai = func() *time.Location {
	l, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
	return l
}()

func TestSalesSnapshot(t *testing.T) {
	// 上海时间 2026-10-17 12:00，当日窗口 18 日零点重置。
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, shanghai).UTC()
	st := fakeSalesStore{
		ok:       true,
		received: now.Add(-2 * time.Minute),
		rep: sales.Report{
			Date: "2026-10-17",
			TZ:   "Asia/Shanghai",
			GMV:  &sales.Metric{Value: 146_000, Goal: 200_000},
			// 订单超额完成。
			Orders:      &sales.Metric{Value: 420, Goal: 400},
			CardRevenue: &sales.Metric{Value: 8_000, Goal: 10_000},
		},
	}
	s, err := NewSales(st, NewFixedTZ("UTC")).Snapshot(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Now.Location().String() != "Asia/Shanghai" {
		t.Errorf("时区 = %v，要 Asia/Shanghai", s.Now.Location())
	}
	if !s.LastUpdate.Equal(st.received) || !s.HasData() {
		t.Errorf("上报时刻不对: %v", s.LastUpdate)
	}
	// 默认只上成交额与订单，权益卡收入存着但不上屏。
	if len(s.Windows) != 2 {
		t.Fatalf("窗口数 = %d，要 2: %+v", len(s.Windows), s.Windows)
	}
	gmv, ord := s.Windows[0], s.Windows[1]
	if gmv.Label != "今日成交额" || ord.Label != "今日订单" {
		t.Errorf("窗口顺序/名字不对: %q %q", gmv.Label, ord.Label)
	}
	for _, w := range s.Windows {
		if w.Dir != Target {
			t.Errorf("%s 方向应为 Target", w.Label)
		}
		if w.Length != 24*time.Hour {
			t.Errorf("%s 窗口长度应为一天，得到 %v", w.Label, w.Length)
		}
		want := time.Date(2026, 10, 18, 0, 0, 0, 0, shanghai)
		if !w.ResetsAt.Equal(want) {
			t.Errorf("%s 重置时刻 = %v，要 %v", w.Label, w.ResetsAt, want)
		}
	}
	if gmv.UsedPercentage != .73 {
		t.Errorf("成交额达成率 = %v，要 0.73", gmv.UsedPercentage)
	}
	if ord.UsedPercentage != 1.05 {
		t.Errorf("订单达成率 = %v，要 1.05（超额要如实超过 1）", ord.UsedPercentage)
	}
}

// 跨零点后旧数据不能继续挂成「今日」：达成率归零，但保留上报时刻，
// 让底栏的「更新 X 前」说出实话。
func TestSalesStaleDayShowsZero(t *testing.T) {
	now := time.Date(2026, 10, 18, 0, 5, 0, 0, shanghai).UTC()
	st := fakeSalesStore{
		ok:       true,
		received: time.Date(2026, 10, 17, 23, 55, 0, 0, shanghai),
		rep: sales.Report{
			Date: "2026-10-17", TZ: "Asia/Shanghai",
			GMV:    &sales.Metric{Value: 198_000, Goal: 200_000},
			Orders: &sales.Metric{Value: 390, Goal: 400},
		},
	}
	s, err := NewSales(st, NewFixedTZ("UTC")).Snapshot(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Windows) != 2 {
		t.Fatalf("窗口数 = %d", len(s.Windows))
	}
	for _, w := range s.Windows {
		if w.UsedPercentage != 0 {
			t.Errorf("%s 用了昨天的数据: %v", w.Label, w.UsedPercentage)
		}
	}
	if !s.HasData() {
		t.Error("跨天归零不等于没数据，LastUpdate 要保留")
	}
}

// 没设目标的指标画不出达成率，不上屏；剩下的窗口照常。
func TestSalesSkipsMetricWithoutGoal(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, shanghai).UTC()
	st := fakeSalesStore{
		ok: true, received: now,
		rep: sales.Report{
			Date: "2026-10-17", TZ: "Asia/Shanghai",
			GMV:    &sales.Metric{Value: 146_000, Goal: 0},
			Orders: &sales.Metric{Value: 420, Goal: 400},
		},
	}
	s, err := NewSales(st, NewFixedTZ("UTC")).Snapshot(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Windows) != 1 || s.Windows[0].Label != "今日订单" {
		t.Fatalf("只该剩订单窗口: %+v", s.Windows)
	}
}

// 还没上报过时要出「等待上报」的快照，不能报错——否则刚配对的设备没帧可取。
func TestSalesNoReport(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	s, err := NewSales(fakeSalesStore{}, NewFixedTZ("Asia/Shanghai")).Snapshot(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Windows) != 0 || s.HasData() {
		t.Fatalf("没上报过不该有窗口: %+v", s)
	}
	if s.Dir != Target {
		t.Error("空快照也要带方向，否则占位文案会用额度口径")
	}
	if s.Now.Location().String() != "Asia/Shanghai" {
		t.Errorf("没上报时时区要回退到默认值，得到 %v", s.Now.Location())
	}
}

// 换 Show 就能把权益卡收入顶上屏，不用改渲染器。
func TestSalesShowCardRevenue(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, shanghai).UTC()
	src := NewSales(fakeSalesStore{
		ok: true, received: now,
		rep: sales.Report{
			Date: "2026-10-17", TZ: "Asia/Shanghai",
			GMV:         &sales.Metric{Value: 146_000, Goal: 200_000},
			CardRevenue: &sales.Metric{Value: 8_000, Goal: 10_000},
		},
	}, NewFixedTZ("UTC"))
	src.Show = [2]sales.Kind{sales.KindCardRevenue, sales.KindGMV}
	s, err := src.Snapshot(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Windows) != 2 || s.Windows[0].Label != "权益卡收入" {
		t.Fatalf("Show 没生效: %+v", s.Windows)
	}
	if s.Windows[0].UsedPercentage != .8 {
		t.Errorf("权益卡达成率 = %v，要 0.8", s.Windows[0].UsedPercentage)
	}
}
