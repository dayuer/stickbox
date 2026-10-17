package source

import (
	"context"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/usage"
)

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
