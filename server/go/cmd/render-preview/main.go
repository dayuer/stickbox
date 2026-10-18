// render-preview 出 5 种状态的看板帧，并与设计稿截图并排拼成对照图。
//
//	go run ./cmd/render-preview -out docs/design/preview
//
// 固定时钟 2026-10-17 12:47（周六），和设计稿截图一致；数据也按截图里的
// 数值构造，便于逐区域人工比对。输出不参与构建，也不需要板子。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/dayuer/stickbox/server/go/internal/frame"
	"github.com/dayuer/stickbox/server/go/internal/render"
	"github.com/dayuer/stickbox/server/go/internal/source"
)

var previewZone = time.FixedZone("CST", 8*3600)

type scenario struct {
	name string
	has  bool
	u5   float64
	rem5 time.Duration
	u7   float64
	rem7 time.Duration
}

var scenarios = []scenario{
	{"01-超速", true, .02, 4*time.Hour + 15*time.Minute + 30*time.Second, .34, 4*24*time.Hour + 18*time.Hour + 13*time.Minute},
	{"02-安全", true, .38, 2*time.Hour + 24*time.Minute + 2*time.Second, .20, 4*24*time.Hour + 18*time.Hour + 13*time.Minute},
	{"03-危险", true, .92, time.Hour + 4*time.Minute + 55*time.Second, .34, 4*24*time.Hour + 18*time.Hour + 13*time.Minute},
	{"04-用满", true, 1, 42*time.Minute + 15*time.Second, .364, 4*24*time.Hour + 16*time.Hour},
	{"05-无数据", false, 0, 0, 0, 0},
}

func main() {
	out := flag.String("out", filepath.Join("..", "..", "docs", "design", "preview"), "输出目录")
	design := flag.String("design", filepath.Join("..", "..", "docs", "design", "screenshots"), "设计稿截图目录")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	rr, err := render.New()
	if err != nil {
		fatal(err)
	}
	th := render.Quota{R: rr}

	now := time.Date(2026, 10, 17, 12, 47, 0, 0, previewZone)
	for _, sc := range scenarios {
		snap := snapshot(sc, now)

		// 设计稿参考：直接渲染的整帧（自适应质量，≤120KB 预览）
		full, err := render.EncodeFrame(th.FullImage(snap))
		if err != nil {
			fatal(fmt.Errorf("%s: %w", sc.name, err))
		}
		fullPath := filepath.Join(*out, sc.name+".jpg")
		if err := os.WriteFile(fullPath, full, 0o644); err != nil {
			fatal(err)
		}

		// 设备实际会拿到的包：底帧 + 图集 + 元数据 + 当前时间拼接帧
		b, err := frame.Build(th, snap)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", sc.name, err))
		}
		meta, err := b.MetaJSON()
		if err != nil {
			fatal(err)
		}
		spliced, err := b.Splice(now.Hour(), now.Minute())
		if err != nil {
			fatal(fmt.Errorf("%s 拼接: %w", sc.name, err))
		}
		if err := os.WriteFile(filepath.Join(*out, sc.name+".base.jpg"), b.Base, 0o644); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(filepath.Join(*out, sc.name+".atlas.bin"), b.Atlas, 0o644); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(filepath.Join(*out, sc.name+".meta.json"), meta, 0o644); err != nil {
			fatal(err)
		}
		splicedPath := filepath.Join(*out, sc.name+".spliced.jpg")
		if err := os.WriteFile(splicedPath, spliced, 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("%s  设计参考 %d B；底帧 %d B + 图集 %d B + 元数据 %d B；拼接帧 %d B\n",
			sc.name, len(full), len(b.Base), len(b.Atlas), len(meta), len(spliced))
		fmt.Printf("        %s\n        %s\n", fullPath, splicedPath)

		shot := filepath.Join(*design, sc.name+".png")
		if _, err := os.Stat(shot); err != nil {
			continue
		}
		cmpPath := filepath.Join(*out, sc.name+".compare.jpg")
		if err := writeCompare(cmpPath, spliced, shot); err != nil {
			fatal(fmt.Errorf("%s 对照图: %w", sc.name, err))
		}
		fmt.Printf("        对照图 %s\n", cmpPath)
	}
}

func snapshot(sc scenario, now time.Time) source.Snapshot {
	s := source.Snapshot{Now: now, LastUpdate: now.Add(-3 * time.Minute), DeviceIP: "192.168.3.14"}
	if !sc.has {
		return s
	}
	s.Windows = []source.Window{
		{Label: "5 小时", Length: 5 * time.Hour, UsedPercentage: sc.u5, ResetsAt: now.Add(sc.rem5)},
		{Label: "7 天", Length: 7 * 24 * time.Hour, UsedPercentage: sc.u7, ResetsAt: now.Add(sc.rem7)},
	}
	return s
}

// writeCompare 左：服务端渲染帧（1920×1080）；右：设计稿截图等比放大到 1080 高。
func writeCompare(path string, frame []byte, shotPath string) error {
	rendered, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		return err
	}
	f, err := os.Open(shotPath)
	if err != nil {
		return err
	}
	shot, err := png.Decode(f)
	f.Close()
	if err != nil {
		return err
	}

	const w, h = 2 * 1920, 1080
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.Black), image.Point{}, xdraw.Src)
	// 左边渲染帧
	xdraw.Draw(canvas, image.Rect(0, 0, 1920, 1080), rendered, image.Point{}, xdraw.Src)
	// 右边设计稿：540 高 → 1080 高，宽 1848，水平居中
	scaled := image.NewRGBA(image.Rect(0, 0, 1848, 1080))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), shot, shot.Bounds(), xdraw.Src, nil)
	xdraw.Draw(canvas, image.Rect(1956, 0, 1956+1848, 1080), scaled, image.Point{}, xdraw.Src)

	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	return jpeg.Encode(out, canvas, &jpeg.Options{Quality: 88})
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "render-preview:", err)
	os.Exit(1)
}
