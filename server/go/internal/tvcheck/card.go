// Package tvcheck 是 StickBox 电视兼容性检测工具的核心。
//
// 它在用户电脑上发现局域网里的 DLNA 渲染器，推一条 1080p MJPEG/AVI 测试流，
// 记录电视的 SOAP 返回码、起播耗时、长跑断流与 HTTP 请求头，最后输出
// JSON 报告和可直接贴进 GitHub issue 的 Markdown 摘要。
package tvcheck

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/dayuer/stickbox/server/go/internal/jpegrestart"
	"github.com/dayuer/stickbox/server/go/internal/render"
)

const (
	cardW = 1920
	cardH = 1080
)

// Card 是每秒刷新一次的测试卡。Current 可被推流 goroutine 并发调用。
type Card struct {
	subtitle string
	face     font.Face
	title    font.Face
	small    font.Face
	clock    font.Face

	mu   sync.Mutex
	jpeg []byte
}

// NewCard 解析内嵌中文字体并先出一帧，避免电视拿到空画面。
func NewCard(name string) (*Card, error) {
	raw, err := render.FontMedium()
	if err != nil {
		return nil, fmt.Errorf("读取测试卡字体：%w", err)
	}
	f, err := opentype.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("解析测试卡字体：%w", err)
	}
	face := func(size float64) (font.Face, error) {
		return opentype.NewFace(f, &opentype.FaceOptions{
			Size: size, DPI: 72, Hinting: font.HintingFull,
		})
	}
	c := &Card{subtitle: name}
	if c.title, err = face(84); err != nil {
		return nil, err
	}
	if c.face, err = face(40); err != nil {
		return nil, err
	}
	if c.small, err = face(28); err != nil {
		return nil, err
	}
	if c.clock, err = face(210); err != nil {
		return nil, err
	}
	if err := c.Update(time.Now()); err != nil {
		return nil, err
	}
	return c, nil
}

// Current 返回最新一帧 JPEG。
func (c *Card) Current() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jpeg
}

// Update 按当前时间重画并编码。
func (c *Card) Update(t time.Time) error {
	img := image.NewRGBA(image.Rect(0, 0, cardW, cardH))
	bg := color.RGBA{0x1d, 0x2d, 0x3d, 0xff}
	grid := color.RGBA{0x2c, 0x45, 0x5d, 0xff}
	ink := color.RGBA{0xf5, 0xf5, 0xf8, 0xff}
	muted := color.RGBA{0x94, 0xbc, 0xe3, 0xff}
	accent := color.RGBA{0x4c, 0xc3, 0x8a, 0xff}

	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	for x := 0; x < cardW; x += 120 {
		draw.Draw(img, image.Rect(x, 0, x+1, cardH), &image.Uniform{grid}, image.Point{}, draw.Src)
	}
	for y := 0; y < cardH; y += 120 {
		draw.Draw(img, image.Rect(0, y, cardW, y+1), &image.Uniform{grid}, image.Point{}, draw.Src)
	}

	drawCentered(img, c.title, "StickBox 兼容性测试", 220, ink)
	drawCentered(img, c.face, "TV Compatibility Check", 300, muted)
	drawCentered(img, c.clock, t.Format("15:04:05"), 560, ink)
	drawCentered(img, c.face, t.Format("2006-01-02"), 650, muted)

	if c.subtitle != "" {
		drawCentered(img, c.small, c.subtitle, 760, muted)
	}
	drawCentered(img, c.small, "请保持此画面，测试结束后会自动停止", 940, muted)

	// 每秒走一格的进度条：画面如果卡住，一眼能看出来。
	progress := float64(t.Second()) / 60
	barW := 900
	x0 := (cardW - barW) / 2
	draw.Draw(img, image.Rect(x0, 860, x0+barW, 868), &image.Uniform{grid}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(x0, 860, x0+int(progress*float64(barW)), 868),
		&image.Uniform{accent}, image.Point{}, draw.Src)

	var encoded []byte
	if res, err := jpegrestart.Encode(img, jpegrestart.Options{Quality: 85, RestartInterval: 8}); err == nil {
		encoded = res.JPEG
	} else {
		var buf jpegBuf
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
			return err
		}
		encoded = buf
	}
	c.mu.Lock()
	c.jpeg = encoded
	c.mu.Unlock()
	return nil
}

// jpegBuf 让标准库编码也能直接写进 []byte。
type jpegBuf []byte

func (b *jpegBuf) Write(p []byte) (int, error) {
	*b = append(*b, p...)
	return len(p), nil
}

func drawCentered(dst *image.RGBA, face font.Face, text string, baseline int, col color.Color) {
	w := font.MeasureString(face, text).Ceil()
	x := (dst.Bounds().Dx() - w) / 2
	d := font.Drawer{
		Dst:  dst,
		Src:  &image.Uniform{col},
		Face: face,
		Dot:  fixed.P(x, baseline),
	}
	d.DrawString(text)
}
