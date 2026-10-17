// Package source 是看板的数据源插件层。
//
// 渲染器只认识 Snapshot，不认识 Claude、也不认识 HTTP。新增一个数据源
// （别的订阅服务、构建状态、天气）只要实现 Source，把窗口换算成
// 已用比例与重置时刻即可，渲染器不用改。
package source

import (
	"context"
	"time"
)

// Window 是一个额度窗口，渲染器按 Length 区分主/次候选。
type Window struct {
	// Label 显示在环内，例如 "5 小时" / "7 天"。
	Label string
	// Length 是窗口总长，决定时间进度与建议上限的单位。
	Length time.Duration
	// UsedPercentage 是 0..1 的已用比例（注意不是百分数）。
	UsedPercentage float64
	// ResetsAt 是窗口重置的绝对时刻。
	ResetsAt time.Time
}

// Snapshot 是某一时刻要落到一帧里的全部数据。
type Snapshot struct {
	// Now 是渲染时刻，时钟与倒计时都以它为准。
	Now time.Time
	// Windows 最多两个；少于两个时缺的位置按「无数据」画。
	Windows []Window
	// LastUpdate 是数据真正到达服务端的时刻，用于「更新 X 分前」。
	// 零值表示从未收到过上报。
	LastUpdate time.Time
	// DeviceIP 只在完全没有数据时显示在底栏。
	DeviceIP string
}

// Source 产出一帧所需的数据。now 由调用方传入，便于测试固定时钟。
type Source interface {
	ID() string
	Snapshot(ctx context.Context, now time.Time) (Snapshot, error)
}
