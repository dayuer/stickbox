// Package render 把一份看板快照画成 1920×1080 的 baseline JPEG。
//
// 版面来自 docs/design/README.md 与设计稿 HTML：钢蓝底、120px 网格、
// 「+」角标面板、270° 用量环、翻页时钟（静态帧，不做翻页动画）。
//
// 渲染是纯函数：同样的 Snapshot 永远出同样的字节。设备侧按分钟轮换
// flash 槽位，服务端只需要在数据或时钟变化时重新出帧。
package render

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/jpeg"
	"time"

	"golang.org/x/image/font/opentype"

	"github.com/dayuer/stickbox/server/go/internal/metrics"
	"github.com/dayuer/stickbox/server/go/internal/source"
)

// 一帧的预算。设备 flash 每槽 128KB，留出 8KB 余量。
const (
	TargetFrameBytes  = 120 << 10
	HardMaxFrameBytes = 128 << 10
)

//go:embed fonts/*.ttf
var fontFS embed.FS

// Renderer 持有解析好的字体，可并发复用（渲染本身不写共享状态）。
type Renderer struct {
	r *renderer
}

// New 加载内嵌字体。
func New() (*Renderer, error) {
	files := map[fontKind]string{
		fontBarlowCondensed: "fonts/BarlowCondensed-SemiBold.ttf",
		fontBarlowBold:      "fonts/Barlow-Bold.ttf",
		fontNotoMedium:      "fonts/NotoSansSC-Medium.ttf",
		fontNotoBold:        "fonts/NotoSansSC-Bold.ttf",
	}
	fonts := make(map[fontKind]*opentype.Font, len(files))
	for kind, name := range files {
		b, err := fontFS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("render: 读字体 %s: %w", name, err)
		}
		f, err := opentype.Parse(b)
		if err != nil {
			return nil, fmt.Errorf("render: 解析字体 %s: %w", name, err)
		}
		fonts[kind] = f
	}
	return &Renderer{r: &renderer{fonts: fonts}}, nil
}

// Render 出一帧 JPEG。质量从 90 起逐级下降，直到不超过 TargetFrameBytes；
// 最低质量仍超过 HardMaxFrameBytes 时返回错误——宁可不出帧，也不能写爆槽位。
func (rr *Renderer) Render(s source.Snapshot) ([]byte, error) {
	start := time.Now()
	b, err := encodeJPEG(rr.FullImage(s))
	metrics.ObserveRender(time.Since(start), err == nil)
	return b, err
}

// FullImage 是带当前时钟数字的完整画面（预览、对照、非局部刷新路径用）。
func (rr *Renderer) FullImage(s source.Snapshot) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	rr.paint(img, s, true)
	return img
}

// BaseImage 是写进 flash 的底帧：四张数字卡片留空，由设备换入字形图集。
func (rr *Renderer) BaseImage(s source.Snapshot) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	rr.paint(img, s, false)
	return img
}

// CardTile 是 128×192 的单张卡片（底 + 铰链 + 数字），用于字形图集。
func (rr *Renderer) CardTile(digit byte) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, clockCardW, clockCardH))
	rr.r.drawClockCard(img, 0, 0, clockCardW, clockCardH, string(digit), true)
	return img
}

func (rr *Renderer) paint(dst *image.RGBA, s source.Snapshot, drawDigits bool) {
	fillRect(dst, 0, 0, 1920, 1080, colBG)
	// 120px 网格，偏移 (72,40)
	for x := 72; x < 1920; x += 120 {
		vline(dst, x, 0, 1080, colGrid)
	}
	for y := 40; y < 1080; y += 120 {
		hline(dst, 0, 1920, y, colGrid)
	}

	main, secondary := views(s)
	r := rr.r
	r.drawHeader(dst, s, main)
	r.drawClock(dst, s.Now, drawDigits)
	r.drawMain(dst, main)
	r.drawSecondary(dst, secondary)
	r.drawLegend(dst)
	r.drawFooter(dst, s, main, secondary)
}

func encodeJPEG(img image.Image) ([]byte, error) {
	var (
		last  []byte
		lastQ int
	)
	for q := 90; q >= 40; q -= 5 {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, fmt.Errorf("render: 编码 JPEG: %w", err)
		}
		last, lastQ = buf.Bytes(), q
		if len(last) <= TargetFrameBytes {
			return last, nil
		}
	}
	if len(last) > HardMaxFrameBytes {
		return nil, fmt.Errorf("render: 帧 %d 字节，超过硬上限 %d（质量 %d）",
			len(last), HardMaxFrameBytes, lastQ)
	}
	return last, nil
}

func (r *renderer) ascent(k fontKind, size float64) float64 {
	f, err := r.face(k, size)
	if err != nil {
		return size * .8
	}
	defer f.Close()
	return float64(f.Metrics().Ascent) / 64
}

func (r *renderer) drawHeader(dst *image.RGBA, s source.Snapshot, p windowView) {
	const x, top = 72, 64
	date := fmt.Sprintf("%d月%d日 周%s", int(s.Now.Month()), s.Now.Day(), weekday(s.Now))
	r.drawText(dst, x, top, 26, 26, .12, colMinor,
		splitRuns("CLAUDE 用量 · "+date, fontBarlowCondensed, fontNotoMedium)...)

	text, c := headline(p)
	r.drawText(dst, x, top+26+14, 76, 76*1.05, -.01, c, textRun{text, fontNotoBold})
}

func (r *renderer) drawFooter(dst *image.RGBA, s source.Snapshot, main, secondary windowView) {
	const left, right, top = 72, 1848, 996
	bothNone := main.none && secondary.none

	var leftText string
	leftColor := colMinor
	if bothNone {
		leftText = "尚未收到上报"
		if s.DeviceIP != "" {
			leftText += " · " + s.DeviceIP
		}
	} else {
		mins := 0
		if !s.LastUpdate.IsZero() {
			mins = int(mathFloor(s.Now.Sub(s.LastUpdate).Minutes()))
			if mins < 0 {
				mins = 0
			}
		}
		leftText = fmt.Sprintf("更新 %d分前 · 每 10 分钟上报", mins)
		if mins >= 15 {
			leftColor = colWarn
		}
	}
	r.drawText(dst, left, top, 26, 26, 0, leftColor, textRun{leftText, fontNotoMedium})

	rightText := "预测按当前平均速度"
	if bothNone {
		rightText = "POST /api/usage"
	}
	r.drawRight(dst, right, top, 26, 26, 0, colMinor, textRun{rightText, fontNotoMedium})
}

func weekday(t time.Time) string {
	return string([]rune("日一二三四五六")[int(t.Weekday())])
}

// mathFloor 单独包一层，避免在 render.go 里引 math 只为一个调用。
func mathFloor(v float64) float64 {
	if v >= 0 {
		return float64(int64(v))
	}
	return float64(int64(v) - 1)
}
