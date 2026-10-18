package frames

import (
	"image"

	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/theme"
)

// stubTheme 是单测用的假主题：只提供 ID、数据源名和一份合法几何，
// 不真的画图（这些测试验的是存储、版本、播放列表，不是渲染）。
//
// 几何用的是和额度看板一样的那组数，所以 Geometry.Validate 能过。
type stubTheme struct {
	id  string
	src string
}

func (t stubTheme) ID() string     { return t.id }
func (t stubTheme) Source() string { return t.src }

func (stubTheme) Geometry() theme.Geometry {
	return theme.Geometry{
		BandRow0: 4, BandRow1: 16,
		Slots: [theme.NumSlots]theme.ClockSlot{
			{X: 1152, Y: 64, W: 128, H: 192, MCUCol: 72},
			{X: 1280, Y: 64, W: 128, H: 192, MCUCol: 80},
			{X: 1536, Y: 64, W: 128, H: 192, MCUCol: 96},
			{X: 1664, Y: 64, W: 128, H: 192, MCUCol: 104},
		},
	}
}

func (stubTheme) BaseImage(source.Snapshot) *image.RGBA {
	return image.NewRGBA(image.Rect(0, 0, theme.Width, theme.Height))
}

func (stubTheme) FullImage(source.Snapshot) *image.RGBA {
	return image.NewRGBA(image.Rect(0, 0, theme.Width, theme.Height))
}

func (stubTheme) CardTile(byte) *image.RGBA {
	return image.NewRGBA(image.Rect(0, 0, 128, 192))
}

var _ theme.Theme = stubTheme{}
