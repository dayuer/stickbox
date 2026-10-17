package source

import (
	"context"
	"log/slog"
	"sort"
	"time"
)

// Composite 把多路数据源合成一路，按**有数据的源的数量**决定看板形态。
//
// 版面固定两个环，所以：
//
//	0 路有数据  →  第一路的占位快照（「等待上报」那张帧，文案口径跟着它）
//	1 路有数据  →  这一路独占两个环（5 小时 + 7 天 / 成交额 + 订单）
//	2 路有数据  →  各占一个环，每路只出它最重要的那个窗口
//	3 路以上    →  取最近上报的两路，仍各占一个环
//
// 所以「这块屏显示什么」不需要配置项、也不需要在用户表加字段：agent 报
// 几路就决定了看板形态。只报 /api/usage 看到完整额度看板，再报 /api/data
// 就变成额度 + 经营各一环。
//
// 每路「最重要的那个窗口」取 Windows[0]：Source 的实现必须把窗口按重要性
// 排好（Claude 是 5 小时在前，经营数据是 Show 的顺序）。
//
// 代价是每次取快照要问遍所有源。所以 Source 的实现必须是便宜的本地读
// （见 Source 的文档）——巡检每分钟会对每个用户问一轮。
type Composite struct {
	Sources []Source
}

func NewComposite(srcs ...Source) Composite { return Composite{Sources: srcs} }

func (c Composite) ID() string {
	if len(c.Sources) == 0 {
		return "empty"
	}
	id := c.Sources[0].ID()
	for _, s := range c.Sources[1:] {
		id += "+" + s.ID()
	}
	return id
}

func (c Composite) Snapshot(ctx context.Context, userID string, now time.Time) (Snapshot, error) {
	var ok []Snapshot      // 成功返回的，按源顺序
	var contrib []Snapshot // 其中真有窗口可画的
	var firstErr error

	for _, s := range c.Sources {
		snap, err := s.Snapshot(ctx, userID, now)
		if err != nil {
			// 一路挂了不该把另一路也拖下水：显示还能用的那块板
			// 比显示空白好。全挂了才往上报错。
			slog.Warn("取数据源快照失败", "source", s.ID(), "user", userID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok = append(ok, snap)
		if len(snap.Windows) > 0 {
			contrib = append(contrib, snap)
		}
	}
	if len(ok) == 0 {
		return Snapshot{}, firstErr
	}

	switch len(contrib) {
	case 0:
		// 一个环都画不出来。优先用「收到过数据但没窗口可画」的那一路
		// （例如经营数据一个目标都没设），它的 LastUpdate 能让底栏说实话。
		for _, s := range ok {
			if s.HasData() {
				return s, nil
			}
		}
		return ok[0], nil
	case 1:
		return contrib[0], nil
	}

	// 两路以上：各让一个环。超过两路时取最近上报的两路。
	sort.SliceStable(contrib, func(i, j int) bool {
		return contrib[i].LastUpdate.After(contrib[j].LastUpdate)
	})
	a, b := contrib[0], contrib[1]
	merged := Snapshot{
		Now:      a.Now, // 时钟用最新那路的时区
		Windows:  []Window{a.Windows[0], b.Windows[0]},
		DeviceIP: a.DeviceIP,
		Dir:      a.Windows[0].Dir,
	}
	// 底栏只有一个「更新 X 前」，而屏上有两路数据。取更旧的那个：
	// 宁可说「这上面有东西已经这么旧了」，也不能用新鲜的那路把旧的盖掉。
	merged.LastUpdate = b.LastUpdate
	if a.LastUpdate.Before(b.LastUpdate) {
		merged.LastUpdate = a.LastUpdate
	}
	return merged, nil
}
