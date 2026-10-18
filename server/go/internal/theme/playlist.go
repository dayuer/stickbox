package theme

import (
	"fmt"
	"time"
)

// 轮播时长的边界。太短电视来不及把一张帧完整显示出来（推流本身有节奏，
// 还要留出设备换包、重拼时钟带的时间）；太长就不叫轮播了。
const (
	MinDwell     = 5 * time.Second
	MaxDwell     = 10 * time.Minute
	DefaultDwell = 20 * time.Second
)

// Slot 是播放列表的一项：放哪个主题、它的帧包版本、停留多久。
type Slot struct {
	ID string `json:"id"`
	// Ver 是这个主题当前帧包的版本。设备拿它做等值比较决定要不要重取，
	// 所以必须按 (用户, 主题) 单调递增，绝不回退。
	Ver uint64 `json:"ver"`
	// Secs 是停留秒数。
	Secs int `json:"secs"`
}

// Playlist 是服务端下发给设备的轮播表，**按优先级排序**。
//
// 服务端不裁剪长度：它不知道对面那块板的 flash 有多大（机群可能 8MB 和
// 16MB 混着）。设备自己按 framepack_slots()/FP_SLOTS_PER_THEME 算出能驻留
// 几个主题，从前往后留下它装得下的那些。所以排在前面的主题优先上屏。
//
// 只有一项时就是「不轮播」，和加主题之前的行为一致。
type Playlist []Slot

// Primary 是主主题：设备至少会显示它，老固件也只认它那一个版本号。
func (p Playlist) Primary() (Slot, bool) {
	if len(p) == 0 {
		return Slot{}, false
	}
	return p[0], true
}

// Find 按主题 ID 取一项。
func (p Playlist) Find(id string) (Slot, bool) {
	for _, s := range p {
		if s.ID == id {
			return s, true
		}
	}
	return Slot{}, false
}

// Total 是跑完一轮的时长，用于日志与自检。
func (p Playlist) Total() time.Duration {
	var d time.Duration
	for _, s := range p {
		d += time.Duration(s.Secs) * time.Second
	}
	return d
}

// Validate 在下发之前把设备会当成硬错误的情况全挡掉。
func (p Playlist) Validate() error {
	if len(p) == 0 {
		return fmt.Errorf("theme: 播放列表为空")
	}
	seen := make(map[string]bool, len(p))
	for i, s := range p {
		if s.ID == "" {
			return fmt.Errorf("theme: 第 %d 项缺主题 ID", i)
		}
		if seen[s.ID] {
			return fmt.Errorf("theme: 主题 %q 重复", s.ID)
		}
		seen[s.ID] = true
		if s.Ver == 0 {
			return fmt.Errorf("theme: 主题 %q 的版本为 0", s.ID)
		}
		d := time.Duration(s.Secs) * time.Second
		if d < MinDwell || d > MaxDwell {
			return fmt.Errorf("theme: 主题 %q 停留 %ds 超出 [%s, %s]",
				s.ID, s.Secs, MinDwell, MaxDwell)
		}
	}
	return nil
}

// ClampDwell 把停留时长收进合法区间；0 取默认值。
func ClampDwell(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return DefaultDwell
	case d < MinDwell:
		return MinDwell
	case d > MaxDwell:
		return MaxDwell
	}
	return d
}
