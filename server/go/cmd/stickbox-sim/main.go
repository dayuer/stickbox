// stickbox-sim 是一台假设备：注册 → 配对 → 收帧 → 拼接 → MJPEG/AVI 推流。
//
// 它按固件 T13b 的规则实现（docs/FRAME.md），用于端到端测试和电视推流：
//
//	stickbox-sim --server https://mqtt.stickbox.ai --state ./sim-state \
//	             --listen :8090 [--tv 192.168.3.18]
//
// 电视由 --tv 指定才会推 DLNA；不带 --tv 只在本机起 /live.avi。
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
	flag.StringVar(&cfg.Server, "server", "https://mqtt.stickbox.ai", "服务端地址")
	flag.StringVar(&cfg.MQTTAddr, "mqtt", "", "MQTT host:port（默认从 server 推 :8883）")
	flag.StringVar(&cfg.StateDir, "state", "stickbox-sim-state", "设备密钥/配对码持久化目录")
	flag.BoolVar(&cfg.Insecure, "insecure", false, "跳过 TLS 证书校验（本地自签名）")
	flag.StringVar(&cfg.StreamAddr, "listen", ":8090", "MJPEG/AVI 监听地址；留空不启流")
	flag.StringVar(&cfg.TV, "tv", "", "可选：DLNA 电视 IP；不填只起流不推送")
	flag.IntVar(&cfg.FPS, "fps", 2, "推流帧率")
	flag.IntVar(&cfg.Burst, "burst", 8, "开机突发帧数")
	flag.StringVar(&cfg.Dev, "dev", "", "可选：固定设备 ID（默认随机生成并持久化）")
	flag.Parse()

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
