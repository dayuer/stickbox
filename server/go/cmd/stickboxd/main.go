package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/broker"
	"github.com/dayuer/stickbox/server/go/internal/config"
	"github.com/dayuer/stickbox/server/go/internal/frames"
	"github.com/dayuer/stickbox/server/go/internal/httpapi"
	"github.com/dayuer/stickbox/server/go/internal/registry"
	"github.com/dayuer/stickbox/server/go/internal/render"
	"github.com/dayuer/stickbox/server/go/internal/store"
	"github.com/dayuer/stickbox/server/go/internal/tlsx"
	"github.com/dayuer/stickbox/server/go/internal/version"
)

func main() {
	confPath := flag.String("conf", envOr("STICKBOX_CONF", "/opt/stickbox/config.json"),
		"配置文件路径")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, err := config.Load(*confPath)
	if err != nil {
		die("读配置失败", err)
	}
	// 子命令走另一条路，不起服务
	if args := flag.Args(); len(args) > 0 {
		if err := runCLI(cfg, args); err != nil {
			die("命令失败", err)
		}
		return
	}
	if err := serve(cfg); err != nil {
		die("服务退出", err)
	}
}

func serve(cfg *config.Config) error {
	// 数据库文件在 root 下，先把目录建出来。
	if err := os.MkdirAll(cfg.Root, 0o755); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	v := auth.NewVerifier(st)

	reloader, err := tlsx.New(cfg.CertPath(), cfg.KeyPath())
	if err != nil {
		return err
	}

	// 判离线 120 秒：设备保证状态一变 2 秒内推、没变也 30 秒一拍，
	// 连丢四拍才判离线。
	reg := registry.New(registry.Options{IdleTimeout: 120 * time.Second})
	defer reg.Shutdown()

	br, err := broker.New(cfg.MQTTAddr, reloader.Config(), v, reg)
	if err != nil {
		return err
	}

	api := httpapi.New(cfg, st, v, reg, br)
	// 设备订阅 usage 时补发最新一份；usage.Service 要用 broker 发消息，
	// 所以只能在两边都构造完之后接上。
	br.SetUsageResender(api.Usage())

	// 帧服务：渲染器加载内嵌字体；帧包落盘到 root/frames，重启直接加载。
	rr, err := render.New()
	if err != nil {
		return fmt.Errorf("初始化渲染器：%w", err)
	}
	fs, err := frames.New(cfg.Root, st, api.Usage(), br, frames.RenderBuilder{R: rr}, time.Now)
	if err != nil {
		return fmt.Errorf("初始化帧服务：%w", err)
	}
	api.SetFrames(fs)
	br.SetFrameResender(fs)
	dailyCtx, dailyStop := context.WithCancel(context.Background())
	defer dailyStop()
	go fs.RunDaily(dailyCtx)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Handler(),
		TLSConfig:         reloader.Config(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// mochi 的 Serve() 是非阻塞的：启动监听后立即返回 nil。
	// 当成「服务退出」会让进程刚起来就结束——这个 bug 是冒烟测试抓到的，
	// 单测抓不到，因为没有哪个包会去起真的监听。
	if err := br.Serve(); err != nil {
		return err
	}
	slog.Info("MQTT/TLS 监听", "addr", cfg.MQTTAddr)

	errc := make(chan error, 1)
	go func() {
		slog.Info("HTTPS 监听", "addr", cfg.HTTPAddr, "version", version.String())
		err := srv.ListenAndServeTLS("", "") // 证书由 GetCertificate 回调提供
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errc:
		br.Close()
		return err
	case <-sig:
		slog.Info("收到退出信号，优雅关闭")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		br.Close()
		return nil
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func die(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
