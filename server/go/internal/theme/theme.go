// Package theme 是看板模版层。
//
// 一个主题 = 一套定制版面 + 它吃哪一路数据。服务端为每个主题单独渲一个
// 帧包推给设备，设备在同一条视频流里按播放列表轮播。
//
// 设备侧**不认识主题**：跳动带的位置、四张数字卡片的坐标、字形图集全都写在
// 每个帧包自己的 meta.json 里（见 rom/main/framepack.h 的 fp_index_t），
// 所以换一套完全不同的版面不用改固件。固件里写死的只有「形状」：
// 一条带、12 个 MCU 行、4 个数字槽、10 个字形。换句话说，每个主题可以有
// 任意静态背景 + 任意位置的时钟，但**逐分钟跳动的东西只能是那 4 个数字**；
// 想让别的东西也实时跳，就得改固件并重算每个主题的最坏带长。
package theme

import (
	"fmt"
	"image"

	"github.com/dayuer/stickbox/server/go/internal/source"
)

// 画面与 JPEG 的固定量。改这些要同步改固件里的 FP_* 常量。
const (
	Width  = 1920
	Height = 1080
	// MCUCols 是一行多少个 MCU（4:2:0 下 MCU 是 16×16）。
	MCUCols = Width / 16
	// DRI 是 restart interval：每 8 个 MCU 一个 RSTn 标记。D 方案的基础，
	// 卡片必须落在 8 的整数倍列上才能整段替换。
	DRI = 8
	// CardRows 是跳动带占的 MCU 行数（对应 framepack.h 的 FP_BAND_ROWS）。
	CardRows = 12
	// NumSlots 是跳动数字的个数：HH:MM 四位（对应 FP_SLOTS）。
	NumSlots = 4
)

// SlotsPerTheme 是一个主题占几个槽做磨损均衡，必须和固件的
// FP_SLOTS_PER_THEME 一致（rom/main/framepack.h）。设备自己按分区槽数 /
// 每主题槽数算能驻留几个主题；服务端不裁剪播放列表（见 frames.Playlist），
// 所以这里只留常量，不提供 MaxResident。
const SlotsPerTheme = 3

// ClockSlot 是一张跳动数字卡片在底帧里的位置。
type ClockSlot struct {
	X, Y, W, H int
	// MCUCol 是卡片左边缘的 MCU 列，必须是 DRI 的整数倍。
	MCUCol int
}

// Geometry 是一个主题的跳动带几何。设备按它拼段，所以必须和底帧实际画出来
// 的位置严格一致——对不上不会报错，只会在电视上看到错位的数字。
type Geometry struct {
	// BandRow0/BandRow1 是跳动带的 MCU 行区间 [Row0, Row1)。
	BandRow0, BandRow1 int
	Slots              [NumSlots]ClockSlot
}

// Validate 把 D 方案对几何的要求全部检一遍。
//
// 这些约束错了不会在服务端炸，而是在电视上显示成错位或花屏，所以在出帧包
// 之前就拦住。固件侧 fp_index_valid 还会再查一遍（它不信服务端）。
func (g Geometry) Validate() error {
	if g.BandRow1-g.BandRow0 != CardRows {
		return fmt.Errorf("theme: 跳动带 %d 行，要 %d 行", g.BandRow1-g.BandRow0, CardRows)
	}
	if g.BandRow0 < 0 || g.BandRow1*16 > Height {
		return fmt.Errorf("theme: 跳动带 [%d,%d) 超出画面", g.BandRow0, g.BandRow1)
	}
	prev := -1
	for i, s := range g.Slots {
		if s.MCUCol%DRI != 0 {
			return fmt.Errorf("theme: 槽 %d 的 MCU 列 %d 不是 %d 的整数倍", i, s.MCUCol, DRI)
		}
		if s.MCUCol <= prev {
			return fmt.Errorf("theme: 槽 %d 的 MCU 列 %d 没有递增", i, s.MCUCol)
		}
		prev = s.MCUCol
		if s.MCUCol*16 != s.X {
			return fmt.Errorf("theme: 槽 %d 的 x=%d 与 MCU 列 %d 对不上", i, s.X, s.MCUCol)
		}
		if s.Y != g.BandRow0*16 || s.H != CardRows*16 {
			return fmt.Errorf("theme: 槽 %d 的 y=%d h=%d 没有占满跳动带", i, s.Y, s.H)
		}
		if s.W%(DRI*16) != 0 {
			return fmt.Errorf("theme: 槽 %d 的宽 %d 不是整数个 restart 区间", i, s.W)
		}
		if s.MCUCol+s.W/16 > MCUCols {
			return fmt.Errorf("theme: 槽 %d 超出右边缘", i)
		}
	}
	return nil
}

// Theme 是一套定制版面。
//
// 实现住在 internal/render 里（它独占绘图原语与字体）；这里只定契约，
// 免得 render 和 theme 互相 import。
type Theme interface {
	// ID 是主题标识，进播放列表、也做帧包的取用键。
	ID() string
	// Source 声明这个主题吃哪一路数据（source.Source 的 ID）。
	// 播放列表只会排进「这一路真有数据」的主题。
	Source() string
	Geometry() Geometry
	// BaseImage 是写进 flash 的底帧：四张数字卡片留空，由设备换入字形。
	BaseImage(source.Snapshot) *image.RGBA
	// FullImage 是带当前数字的完整画面。两个用处：预览，以及作为
	// 「设备拼出来的帧必须等于整帧渲染」那条对照测试的基准——每个主题
	// 都得过这一条，否则卡片位置和底帧就对不上了。
	FullImage(source.Snapshot) *image.RGBA
	// CardTile 是单张卡片（底 + 数字），用于字形图集。
	// 尺寸必须和 Geometry().Slots 的 W/H 一致。
	CardTile(digit byte) *image.RGBA
}
