package source

import (
	"context"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/usage"
)

// UsageStore 是 Claude 数据源对用量存储的窄依赖。
type UsageStore interface {
	Latest(userID string) (usage.Report, time.Time, bool, error)
}

// ClaudeSource 是 Claude 用量这一路数据源：按 userID 取最近一次上报、
// 按上报里的时区定位，再交给 Claude 做换算。
//
// 时区在这里解析而不是在帧服务里：时区是「这路数据自带的属性」
// （agent 所在机器的时区），别的数据源的时区来源完全不同。
type ClaudeSource struct {
	Store UsageStore
	// DefaultTZ 是上报里没带时区时的兜底（IANA 名）。
	DefaultTZ string
}

// NewClaude 构造 Claude 用量数据源。
func NewClaude(st UsageStore, defaultTZ string) ClaudeSource {
	return ClaudeSource{Store: st, DefaultTZ: defaultTZ}
}

func (ClaudeSource) ID() string { return "claude-usage" }

func (s ClaudeSource) Snapshot(ctx context.Context, userID string, now time.Time) (Snapshot, error) {
	rep, received, ok, err := s.Store.Latest(userID)
	if err != nil {
		return Snapshot{}, err
	}
	if !ok {
		// 刚配对、agent 还没上报过：也要出一张「等待上报」的帧。
		rep, received = usage.Report{}, time.Time{}
	}
	loc := LoadLocation(rep.TZ, s.DefaultTZ)
	return Claude{Report: rep, ReceivedAt: received}.Snapshot(ctx, now.In(loc))
}

// Claude 把一次 agent 上报的用量转换成看板快照。
//
// report 里的百分比是 0..100，这里换算成 0..1；时间戳保持绝对时刻，
// 渲染器自己减去 Now 得到剩余时间——和设计稿的 1 秒 tick 一致。
type Claude struct {
	Report     usage.Report
	ReceivedAt time.Time
	DeviceIP   string
}

func (Claude) ID() string { return "claude-usage" }

func (c Claude) Snapshot(_ context.Context, now time.Time) (Snapshot, error) {
	s := Snapshot{Now: now, LastUpdate: c.ReceivedAt, DeviceIP: c.DeviceIP}
	if w := c.Report.FiveHour; w != nil {
		s.Windows = append(s.Windows, Window{
			Label:          "5 小时",
			Length:         5 * time.Hour,
			UsedPercentage: w.UsedPercentage / 100,
			ResetsAt:       time.Unix(w.ResetsAt, 0),
		})
	}
	if w := c.Report.SevenDay; w != nil {
		s.Windows = append(s.Windows, Window{
			Label:          "7 天",
			Length:         7 * 24 * time.Hour,
			UsedPercentage: w.UsedPercentage / 100,
			ResetsAt:       time.Unix(w.ResetsAt, 0),
		})
	}
	return s, nil
}
