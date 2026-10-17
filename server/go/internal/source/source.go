// Package source 是看板的数据源插件层。
//
// 渲染器只认识 Snapshot，不认识 Claude、也不认识 HTTP。新增一个数据源
// （别的订阅服务、当日销售、构建状态）只要实现 Source：自己取数、自己
// 定时区、把指标换算成窗口，渲染器与帧服务都不用改。
package source

import (
	"context"
	"time"
)

// Direction 是窗口的方向，决定「进度超前」是好事还是坏事。
//
// 环的几何、时间刻度、配速预测三者与方向无关，只有配色和文案要分叉：
// 额度用满是故障，目标达成是业绩。
type Direction int

const (
	// Budget 是消耗型：越少越好，用满=告警。Claude 用量属于这一类。
	Budget Direction = iota
	// Target 是达成型：越多越好，达成=成功。当日订单/GMV 属于这一类。
	Target
)

// Window 是一个按时间推进的进度窗口，渲染器按 Length 区分主/次候选。
type Window struct {
	// Label 显示在环内，例如 "5 小时" / "7 天" / "今日 GMV"。
	Label string
	// Length 是窗口总长，决定时间进度与建议上限的单位。
	Length time.Duration
	// UsedPercentage 是 0..1 的进度比例（注意不是百分数）。
	// Budget 方向是「已用」，Target 方向是「已达成」；两者都可以 >1。
	UsedPercentage float64
	// ResetsAt 是窗口重置的绝对时刻。
	ResetsAt time.Time
	// Dir 决定配色与文案方向，零值 Budget 兼容既有数据源。
	Dir Direction
	// Value/Goal/Unit 是可选的绝对量，用于在环内显示 "12.3万 / 20万"。
	// Goal 为 0 表示没有绝对量可显示，只显示百分比。
	Value float64
	Goal  float64
	Unit  string
}

// Snapshot 是某一时刻要落到一帧里的全部数据。
type Snapshot struct {
	// Now 是渲染时刻，时钟与倒计时都以它为准。
	// 必须带数据源解析出的用户时区：帧服务判断跨天也用它。
	Now time.Time
	// Windows 最多两个；少于两个时缺的位置按「无数据」画。
	Windows []Window
	// LastUpdate 是数据真正到达服务端的时刻，用于「更新 X 分前」。
	// 零值表示这个数据源还没有收到过该用户的数据。
	LastUpdate time.Time
	// DeviceIP 只在完全没有数据时显示在底栏。
	DeviceIP string
}

// HasData 报告这个快照背后是否真有数据到达过。
// 帧服务用它决定要不要按到达时刻做新旧比较。
func (s Snapshot) HasData() bool { return !s.LastUpdate.IsZero() }

// Source 为某个用户产出一帧所需的数据。
//
// 实现负责自己取数：帧服务只传 userID 和 now，不替它准备数据，也不替它
// 解析时区——这样换一路数据不用动帧服务。该用户还没有任何数据时，要返回
// 一个 LastUpdate 为零值的快照（用来画「等待上报」那张帧），而不是报错。
//
// 调用频率：除了每次重渲，帧服务的巡检还会按 StaleCheck（默认 1 分钟）
// 对每个有帧的用户各调一次，只为读 Now 的时区和 LastUpdate。所以实现
// 必须是便宜的本地读——要拉远端就自己在后台拉、这里只读缓存。
type Source interface {
	ID() string
	Snapshot(ctx context.Context, userID string, now time.Time) (Snapshot, error)
}

// LoadLocation 解析 IANA 时区名，失败时依次回退 fallback 与 UTC。
// 各数据源共用，避免每家自己写一遍兜底。
func LoadLocation(tz, fallback string) *time.Location {
	for _, name := range []string{tz, fallback} {
		if name == "" {
			continue
		}
		if l, err := time.LoadLocation(name); err == nil {
			return l
		}
	}
	return time.UTC
}
