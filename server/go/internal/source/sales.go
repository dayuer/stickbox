package source

import (
	"context"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/sales"
)

// SalesStore 是 Sales 数据源对经营数据存储的窄依赖，形状和 UsageStore 一致。
type SalesStore interface {
	Latest(userID string) (sales.Report, time.Time, bool, error)
}

// Sales 是「超代当日经营数据」这一路数据源。
//
// 和 Claude 的区别只有两处：窗口是当日（零点重置）而不是滚动额度窗口，
// 方向是 Target 而不是 Budget。环的几何、配速预测、主次分配完全共用。
type Sales struct {
	Store SalesStore
	// TZ 解析时区：用户设置 > 上报里带的 tz > 服务端默认。
	TZ TZResolver
	// Show 是上屏的两个指标，按顺序决定短窗口/长窗口候选。
	// 版面只有两个环，上报可以带三个指标，由这里挑。
	Show [2]sales.Kind
}

// NewSales 构造经营数据源，默认上屏成交额与订单数。
// 权益卡收入照样会被上报和存下来，换 Show 就能上屏。
func NewSales(st SalesStore, tzr TZResolver) Sales {
	return Sales{
		Store: st,
		TZ:    tzr,
		Show:  [2]sales.Kind{sales.KindGMV, sales.KindOrders},
	}
}

func (Sales) ID() string { return "chaodai-sales" }

func (s Sales) Snapshot(_ context.Context, userID string, now time.Time) (Snapshot, error) {
	rep, received, ok, err := s.Store.Latest(userID)
	if err != nil {
		return Snapshot{}, err
	}
	loc := s.TZ.Location(userID, sales.NormalizeTZ(rep.TZ))
	local := now.In(loc)
	snap := Snapshot{Now: local, Dir: Target}
	if !ok {
		return snap, nil // 还没上报过：画「等待上报」那张帧。
	}
	snap.LastUpdate = received

	// 跨零点之后旧数据不能继续当成「今日」：显示 0 达成 + 「更新 X 小时前」，
	// 比把昨天的成交额挂成今天的要诚实。
	stale := rep.Date != local.Format("2006-01-02")
	reset := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)

	for _, k := range s.Show {
		m := rep.Metric(k)
		if m == nil || m.Goal <= 0 {
			continue // 没设目标就画不出达成率，这一路不上屏。
		}
		u := 0.0
		if !stale {
			u = m.Value / m.Goal
		}
		snap.Windows = append(snap.Windows, Window{
			Label:          k.Label(),
			Length:         24 * time.Hour,
			UsedPercentage: u,
			ResetsAt:       reset,
			Dir:            Target,
		})
	}
	return snap, nil
}
