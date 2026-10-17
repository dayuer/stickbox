package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/broker"
	"github.com/dayuer/stickbox/server/go/internal/config"
	"github.com/dayuer/stickbox/server/go/internal/httpapi"
	"github.com/dayuer/stickbox/server/go/internal/registry"
	"github.com/dayuer/stickbox/server/go/internal/sms"
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

	pb, err := auth.NewPhoneBox(cfg.PhonePepper, cfg.PhoneKey)
	if err != nil {
		return err
	}
	sender, err := newSender(cfg)
	if err != nil {
		return err
	}
	v := auth.NewVerifier(st)
	mem := broker.NewMembership(st)
	smsSvc := auth.NewSMS(st, sender, time.Now)
	if cfg.DevLogin.Enabled() {
		smsSvc.SetDevLogin(pb.HMAC(cfg.DevLogin.Phone), cfg.DevLogin.Code)
		// 每次启动都喊一遍。后门最危险的形态是「配上去之后没人记得」。
		slog.Warn("⚠ 固定手机号登录已启用——这是登录后门，生产环境必须移除",
			"phone", cfg.DevLogin.Phone)
	}

	reloader, err := tlsx.New(cfg.CertPath(), cfg.KeyPath())
	if err != nil {
		return err
	}

	// 判离线 120 秒：设备保证状态一变 2 秒内推、没变也 30 秒一拍，
	// 连丢四拍才判离线。
	reg := registry.New(registry.Options{IdleTimeout: 120 * time.Second})
	defer reg.Shutdown()

	br, err := broker.New(cfg.MQTTAddr, reloader.Config(), v, mem, reg, st)
	if err != nil {
		return err
	}

	api := httpapi.New(cfg, st, v, pb, smsSvc, reg, mem, br)
	// 设备订阅 usage 时补发最新一份；usage.Service 要用 broker 发消息，
	// 所以只能在两边都构造完之后接上。
	br.SetUsageResender(api.Usage())

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

func newSender(cfg *config.Config) (auth.Sender, error) {
	if cfg.SMS.Provider == "aliyun" && cfg.SMS.AccessKeyID != "" {
		return sms.NewAliyun(sms.AliyunConfig{
			AccessKeyID:     cfg.SMS.AccessKeyID,
			AccessKeySecret: cfg.SMS.AccessKeySecret,
			SignName:        cfg.SMS.SignName,
			TemplateCode:    cfg.SMS.TemplateCode,
		})
	}
	slog.Warn("短信未配置，验证码只打日志——生产环境必须配 sms.*")
	return sms.Logger{}, nil
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
