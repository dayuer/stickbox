package render

import (
	"image"

	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/theme"
)

// quotaGeometry 是额度看板的跳动带几何，也是 T13b 的 D 方案基准。
//
// 卡片宽 8 个 MCU、高 12 个 MCU，起始列都是 8 的整数倍（DRI=8 的 restart
// 边界），这样设备能整段替换数字而不用重编码。
// 这里是四张卡片坐标的**唯一来源**：drawClock 画在哪、meta.json 告诉设备
// 去哪换，都从这一份读。以前画图用的是另一份私有坐标表，两份一旦不同步
// 就是电视上错位的数字，而服务端一点错都不报。
var quotaGeometry = theme.Geometry{
	BandRow0: 4, BandRow1: 16, // 第 4..15 MCU 行，y 64..256
	Slots: [theme.NumSlots]theme.ClockSlot{
		{X: 1152, Y: 64, W: 128, H: 192, MCUCol: 72},
		{X: 1280, Y: 64, W: 128, H: 192, MCUCol: 80},
		{X: 1536, Y: 64, W: 128, H: 192, MCUCol: 96},
		{X: 1664, Y: 64, W: 128, H: 192, MCUCol: 104},
	},
}

// Quota 是 Claude 额度看板：双环 + 翻页时钟，消耗型口径。
type Quota struct{ R *Renderer }

func (Quota) ID() string { return "quota" }

func (Quota) Source() string { return "claude-usage" }

func (Quota) Geometry() theme.Geometry { return quotaGeometry }

// BaseImage 是写进 flash 的底帧：四张数字卡片留空，由设备换入字形。
func (q Quota) BaseImage(s source.Snapshot) *image.RGBA {
	return q.paint(s, false)
}

// FullImage 是带当前时钟数字的完整画面（预览、以及拼接对照）。
func (q Quota) FullImage(s source.Snapshot) *image.RGBA {
	return q.paint(s, true)
}

// CardTile 是单张卡片（底 + 铰链 + 数字），用于字形图集。
func (q Quota) CardTile(digit byte) *image.RGBA {
	w, h := quotaGeometry.Slots[0].W, quotaGeometry.Slots[0].H
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	q.R.r.drawClockCard(img, 0, 0, w, h, string(digit), true)
	return img
}

func (q Quota) paint(s source.Snapshot, drawDigits bool) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, theme.Width, theme.Height))
	fillRect(dst, 0, 0, theme.Width, theme.Height, colBG)
	// 120px 网格，偏移 (72,40)
	for x := 72; x < theme.Width; x += 120 {
		vline(dst, x, 0, theme.Height, colGrid)
	}
	for y := 40; y < theme.Height; y += 120 {
		hline(dst, 0, theme.Width, y, colGrid)
	}

	main, secondary := views(s)
	r := q.R.r
	r.drawHeader(dst, s, main)
	r.drawClock(dst, quotaGeometry, s.Now, drawDigits)
	r.drawMain(dst, main)
	r.drawSecondary(dst, secondary)
	r.drawLegend(dst)
	r.drawFooter(dst, s, main, secondary)
	return dst
}

// 编译期确认 Quota 真的满足契约。
var _ theme.Theme = Quota{}
