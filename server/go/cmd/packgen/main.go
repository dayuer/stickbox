// packgen 产出设备真正会收到的 SBF1 帧包，以及若干时刻的黄金拼接帧，
// 供设备侧的 C 实现在主机上逐字节比对（rom/tools/packtest）。
//
// 为什么需要它：T13b 的取帧链路在板上一次都没跑过，而板子不是随时能用。
// 把「服务端产出什么」和「设备会拼出什么」都搬到主机上，就能在烧录之前
// 把字节级的约定钉死——上板只剩下真正只有板子能验的东西（时序、电视行为、
// 内存峰值）。
//
// 和 render-preview 的区别：那个出的是人看的预览图，这个出的是
// frames.Pack() 的**二进制容器**，也就是 /api/device/frame 的响应体原文。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/frame"
	"github.com/dayuer/stickbox/server/go/internal/frames"
	"github.com/dayuer/stickbox/server/go/internal/render"
	"github.com/dayuer/stickbox/server/go/internal/source"
)

var zone = time.FixedZone("CST", 8*3600)

// 拼接要覆盖的时刻。08:28 是实测的最坏用例（四个槽凑齐最宽的数字），
// 00:00 / 23:59 是边界，12:47 和黄金预览图对齐。
var times = [][2]int{{0, 0}, {8, 28}, {12, 47}, {23, 59}}

func main() {
	out := flag.String("out", "/tmp/packgen", "输出目录")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}

	rr, err := render.New()
	if err != nil {
		fatal(err)
	}
	now := time.Date(2026, 10, 17, 12, 47, 0, 0, zone)
	snap := source.Snapshot{
		Now: now, LastUpdate: now.Add(-3 * time.Minute), DeviceIP: "192.168.3.14",
		Windows: []source.Window{
			{Label: "5 小时", Length: 5 * time.Hour, UsedPercentage: .92,
				ResetsAt: now.Add(time.Hour + 4*time.Minute)},
			{Label: "7 天", Length: 7 * 24 * time.Hour, UsedPercentage: .34,
				ResetsAt: now.Add(4*24*time.Hour + 18*time.Hour)},
		},
	}

	b, err := frame.Build(render.Quota{R: rr}, snap)
	if err != nil {
		fatal(err)
	}

	// 1) 设备会收到的那个容器，一个字节不差。
	pack, err := frames.Pack(b)
	if err != nil {
		fatal(err)
	}
	packPath := filepath.Join(*out, "frame.sbf1")
	if err := os.WriteFile(packPath, pack, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("帧包 %s：%d 字节\n", packPath, len(pack))

	// 1b) 拆开的三件套，给 bandtest / metatest 用（它们吃分开的文件）。
	meta, err := b.MetaJSON()
	if err != nil {
		fatal(err)
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{"base.jpg", b.Base}, {"atlas.bin", b.Atlas}, {"meta.json", meta}} {
		if err := os.WriteFile(filepath.Join(*out, f.name), f.data, 0o644); err != nil {
			fatal(err)
		}
	}
	fmt.Printf("  拆开：base %d、atlas %d、meta %d\n", len(b.Base), len(b.Atlas), len(meta))

	// 2) 各时刻的黄金拼接帧。
	for _, t := range times {
		sp, err := b.Splice(t[0], t[1])
		if err != nil {
			fatal(fmt.Errorf("%02d:%02d 拼接: %w", t[0], t[1], err))
		}
		p := filepath.Join(*out, fmt.Sprintf("golden-%02d%02d.jpg", t[0], t[1]))
		if err := os.WriteFile(p, sp, 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("  %02d:%02d → %s（%d 字节）\n", t[0], t[1], filepath.Base(p), len(sp))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
