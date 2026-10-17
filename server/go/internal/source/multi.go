package source

import (
	"context"
	"log/slog"
	"time"
)

// MostRecent 把多路数据源合成一路：谁的数据最新，屏幕就显示谁。
//
// 这样「这块屏显示哪一路」不需要配置项、也不需要在用户表上加字段：
// agent 往 /api/usage 报就看到额度看板，往 /api/data 报就看到经营看板，
// 两边都报就跟着最近一次上报走。版面只有两个环，本来也只能显示一路。
//
// 代价是每次取快照要问遍所有源。所以 Source 的实现必须是便宜的本地读
// （见 Source 的文档）——巡检每分钟会对每个用户问一轮。
type MostRecent struct {
	Sources []Source
}

func NewMostRecent(srcs ...Source) MostRecent { return MostRecent{Sources: srcs} }

func (m MostRecent) ID() string {
	if len(m.Sources) == 0 {
		return "empty"
	}
	id := m.Sources[0].ID()
	for _, s := range m.Sources[1:] {
		id += "+" + s.ID()
	}
	return id
}

func (m MostRecent) Snapshot(ctx context.Context, userID string, now time.Time) (Snapshot, error) {
	var best Snapshot
	var bestAt time.Time
	var firstErr error
	found := false

	for _, s := range m.Sources {
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
		if !found {
			// 一路数据都没有时的兜底：用第一个成功返回的占位快照，
			// 它带着那一路的时区与「等待上报」文案方向。
			best, found = snap, true
		}
		if snap.HasData() && snap.LastUpdate.After(bestAt) {
			best, bestAt = snap, snap.LastUpdate
		}
	}
	if !found {
		return Snapshot{}, firstErr
	}
	return best, nil
}
