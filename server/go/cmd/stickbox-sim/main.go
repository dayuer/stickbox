// stickbox-sim 是一台假设备：注册 → 配对 → 收帧 → 拼接 → MJPEG/AVI 推流。
//
// 它按固件 T13b 的规则实现（docs/FRAME.md），用于端到端测试和电视推流：
//
//	stickbox-sim --server https://mqtt.stickbox.ai --state ./sim-state \
//	             --listen :8090
//
// 加 --push-tv <电视 IP/主机名> 会把 /live.avi 通过 DLNA 推到电视
// （T17 参数：burst 8、2fps、HTTP/1.0、video/avi、Streaming、OP=00、
// FLAGS=017…）。电视先显示配对码，用户在电脑上 stickbox-agent pair
// 完成配对后自动切到真实看板，不需要重新推送。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/dayuer/stickbox/server/go/internal/sim"
)

func main() {
	var cfg sim.Config
	flag.StringVar(&cfg.Server, "server", "https://mqtt.stickbox.ai", "服务端地址（默认正式服务端）")
	flag.StringVar(&cfg.MQTTAddr, "mqtt", "", "MQTT host:port（默认从 server 推 :8883）")
	flag.StringVar(&cfg.StateDir, "state", "stickbox-sim-state", "设备密钥/配对码持久化目录")
	flag.BoolVar(&cfg.Insecure, "insecure", false, "跳过 TLS 证书校验（本地自签名）")
	flag.StringVar(&cfg.StreamAddr, "listen", ":8090", "MJPEG/AVI 监听地址；留空不启流")
	flag.StringVar(&cfg.TV, "push-tv", "", "可选：把画面 DLNA 推到这台电视（IP/主机名）")
	flag.IntVar(&cfg.FPS, "fps", 2, "推流帧率")
	flag.IntVar(&cfg.Burst, "burst", 8, "开机突发帧数")
	flag.StringVar(&cfg.Dev, "dev", "", "可选：固定设备 ID（默认随机生成并持久化）")
	flag.Parse()

	if cfg.TV != "" && cfg.StreamAddr == "" {
		log.Fatal("--push-tv 需要 --listen（电视要能拉到 /live.avi）")
	}

	cfg.Logf = log.Printf
	d, err := sim.New(cfg)
	if err != nil {
		log.Fatalf("初始化失败：%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code, err := d.Register(ctx)
	if err != nil {
		log.Fatalf("注册失败：%v", err)
	}
	if code != "" {
		log.Printf("等待配对：在电脑上运行 stickbox-agent pair %s", code)
	} else {
		log.Printf("设备 %s 已配对", d.Dev())
	}
	if err := d.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("运行失败：%v", err)
	}
	log.Printf("退出")
}
