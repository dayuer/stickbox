// stickbox-tvcheck 在用户电脑上检测电视对 StickBox 推流的兼容性。
//
// 不需要 StickBox 板子：发现局域网里的 DLNA 渲染器，推一条 1080p MJPEG/AVI
// 测试流，记录起播耗时、长跑断流和电视的 HTTP 请求头，最后输出 JSON 报告
// 和可直接贴进 GitHub issue 的 Markdown 摘要。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/tvcheck"
)

func main() {
	var (
		device  = flag.String("device", "", "电视编号、名称、IP 或控制地址（省略则交互选择）")
		minutes = flag.Int("minutes", 3, "持续拉流分钟数")
		fps     = flag.Int("fps", 2, "推流帧率")
		burst   = flag.Int("burst", 8, "起播突发帧数（T17 推荐 8）")
		listen  = flag.String("listen", ":0", "推流监听地址（默认随机端口）")
		jsonOut = flag.String("json", "stickbox-tvcheck.json", "JSON 报告路径")
		mdOut   = flag.String("md", "stickbox-tvcheck.md", "Markdown 摘要路径")
		timeout = flag.Duration("timeout", 3*time.Second, "SSDP 发现时长")
	)
	flag.Parse()

	if *minutes <= 0 {
		log.Fatal("--minutes 必须大于 0")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	renderers, err := tvcheck.Discover(ctx, *timeout)
	if err != nil {
		log.Fatalf("发现电视失败：%v", err)
	}
	sel, err := pick(renderers, *device)
	if err != nil {
		log.Fatal(err)
	}
	if sel.ControlURL == "" {
		log.Fatalf("%s 没有 AVTransport 服务，无法接收推流（可能不是 DLNA 渲染器）", sel.Display())
	}

	log.Printf("目标：%s", sel.Display())
	log.Printf("测试 %d 分钟；按 Ctrl-C 可提前结束", *minutes)
	rep, err := tvcheck.Run(ctx, sel, tvcheck.Options{
		Duration: time.Duration(*minutes) * time.Minute,
		FPS:      *fps,
		Burst:    *burst,
		Listen:   *listen,
		Progress: log.Printf,
	})
	if err != nil {
		log.Fatalf("测试失败：%v", err)
	}

	raw, err := rep.JSON()
	if err != nil {
		log.Fatalf("生成 JSON：%v", err)
	}
	if err := os.WriteFile(*jsonOut, raw, 0o644); err != nil {
		log.Fatalf("写 %s：%v", *jsonOut, err)
	}
	md := rep.Markdown()
	if err := os.WriteFile(*mdOut, []byte(md), 0o644); err != nil {
		log.Fatalf("写 %s：%v", *mdOut, err)
	}

	fmt.Println()
	fmt.Print(md)
	fmt.Printf("\nJSON 报告：%s\nMarkdown 摘要：%s\n", *jsonOut, *mdOut)
	if rep.HasErrors() {
		os.Exit(1)
	}
}

// pick 按 --device 选择；没给就打印列表从标准输入选。
func pick(renderers []tvcheck.Renderer, want string) (tvcheck.Renderer, error) {
	if want != "" {
		if i, err := strconv.Atoi(want); err == nil {
			if i < 1 || i > len(renderers) {
				return tvcheck.Renderer{}, fmt.Errorf("电视编号 %d 超出范围（1-%d）", i, len(renderers))
			}
			return renderers[i-1], nil
		}
		needle := strings.ToLower(want)
		var matches []tvcheck.Renderer
		for _, r := range renderers {
			hay := strings.ToLower(strings.Join([]string{
				r.Name, r.Manufacturer, r.Model, r.IP, r.ControlURL,
			}, " "))
			if strings.Contains(hay, needle) {
				matches = append(matches, r)
			}
		}
		switch len(matches) {
		case 0:
			return tvcheck.Renderer{}, fmt.Errorf("没有匹配 %q 的电视", want)
		case 1:
			return matches[0], nil
		default:
			return tvcheck.Renderer{}, fmt.Errorf("%q 匹配到 %d 台电视，请用编号更精确地选择", want, len(matches))
		}
	}

	fmt.Printf("发现 %d 台 DLNA 渲染器：\n", len(renderers))
	for i, r := range renderers {
		note := ""
		if r.ControlURL == "" {
			note = "  [无 AVTransport，无法测试]"
		}
		fmt.Printf("  %d) %s\n     %s%s\n", i+1, r.Display(), r.ControlURL, note)
	}
	fmt.Printf("请选择 [1-%d]（或回车退出）：", len(renderers))
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return tvcheck.Renderer{}, fmt.Errorf("已取消")
	}
	i, err := strconv.Atoi(line)
	if err != nil || i < 1 || i > len(renderers) {
		return tvcheck.Renderer{}, fmt.Errorf("无效选择 %q", line)
	}
	return renderers[i-1], nil
}
