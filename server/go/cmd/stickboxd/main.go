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
	_ "time/tzdata" // 镜像里没有 tzdata；按用户 IANA 时区渲染需要内嵌一份

	"github.com/dayuer/stickbox/server/go/internal/admin"
	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/broker"
	"github.com/dayuer/stickbox/server/go/internal/command"
	"github.com/dayuer/stickbox/server/go/internal/config"
	"github.com/dayuer/stickbox/server/go/internal/frames"
	"github.com/dayuer/stickbox/server/go/internal/httpapi"
	"github.com/dayuer/stickbox/server/go/internal/metrics"
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

	v := auth.NewVerifier(st, cfg.TokenPepper)
	if os.Getenv("STICKBOX_VERIFIER_CACHE") == "1" {
		// 仅供压测对照，见 task/T25 的提交说明；生产必须走无缓存路径。
		slog.Warn("启用了压测对照的 verifier 缓存（命中仍查库）")
		v = auth.NewVerifierCached(st, cfg.TokenPepper)
	}

	reloader, err := tlsx.New(cfg.CertPath(), cfg.KeyPath())
	if err != nil {
		return err
	}

	// 判离线 120 秒：设备保证状态一变 2 秒内推、没变也 30 秒一拍，
	// 连丢四拍才判离线。
	reg := registry.New(registry.Options{IdleTimeout: 120 * time.Second})
	defer reg.Shutdown()

	br, err := broker.New(cfg.MQTTAddr, reloader.Config(), v, reg, broker.Options{
		MaximumClientWritesPending: cfg.MQTTMaxWritesPending,
		MaximumInflight:            cfg.MQTTMaxInflight,
		ReceiveMaximum:             cfg.MQTTReceiveMaximum,
		AdmitPerSec:                cfg.MQTTAdmitPerSec,
	})
	if err != nil {
		return err
	}

	api := httpapi.New(cfg, st, v, reg, br)
	// /healthz 用它检查 broker 是否还在监听。
	api.SetHealth(br)
	// 设备订阅 usage 时补发最新一份；usage.Service 要用 broker 发消息，
	// 所以只能在两边都构造完之后接上。
	br.SetUsageResender(api.Usage())
	// 设备订阅 pair 时按密钥当前状态补发：retained 只存在 broker 内存里。
	br.SetPairResender(api)

	// 帧服务：渲染器加载内嵌字体；帧包落盘到 root/frames，重启直接加载。
	rr, err := render.New()
	if err != nil {
		return fmt.Errorf("初始化渲染器：%w", err)
	}
	fs, err := frames.New(cfg.Root, st, api.Usage(), br, frames.RenderBuilder{R: rr}, frames.Options{
		DefaultTZ:      cfg.DefaultTZ,
		CacheMaxBytes:  cfg.FrameCacheMaxBytes,
		MemoryMaxBytes: cfg.FrameMemoryMaxBytes,
		BackfillPerSec: cfg.BackfillPerSec,
		Now:            time.Now,
	})
	if err != nil {
		return fmt.Errorf("初始化帧服务：%w", err)
	}
	defer fs.Close()
	api.SetFrames(fs)
	br.SetFrameResender(fs)
	dailyCtx, dailyStop := context.WithCancel(context.Background())
	defer dailyStop()
	go fs.Run(dailyCtx)

	// 下行指令的确认跟踪：带 cid 下发，设备应用完回 ACK，没收到就重投。
	// presence 用 registry——设备离线时不重投，发出去也没人收，白烧重试
	// 预算；等它上线再补（T18）。
	cmdTracker := command.New(br, reg)
	br.SetCommandTracker(cmdTracker)
	cmdDone := make(chan struct{})
	defer close(cmdDone)
	go cmdTracker.Run(cmdDone)

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

	// 指标只监听回环地址，不经过 nginx。online / mqtt 连接数在 scrape 时现查。
	if cfg.MetricsAddr != "" {
		metrics.Default.SetGaugeProviders(reg.OnlineCount, br.ClientCount)
		metrics.Default.SetFramePendingProvider(fs.PendingUsers)
		msrv, err := metrics.Default.Serve(cfg.MetricsAddr)
		if err != nil {
			br.Close()
			return err
		}
		defer msrv.Close()
		slog.Info("metrics 监听", "addr", cfg.MetricsAddr)
	}

	// 设备诊断只监听回环，stickboxd device show / user tz 在服务器本机调用它。
	adminH := admin.NewHandler(st, api.Usage(), fs,
		adminRuntime{br: br, cmd: cmdTracker, api: api})
	// CLI 改时区要立刻重渲并通知设备；它跑在另一个进程，只能走这里。
	adminH.SetTZApplier(adminTZ{st: st, fs: fs})
	adminSrv, err := admin.Serve(cfg.AdminAddr, adminH)
	if err != nil {
		br.Close()
		return err
	}
	defer adminSrv.Close()
	slog.Info("admin 诊断监听", "addr", cfg.AdminAddr)

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

// adminRuntime 把 broker/tracker/API 的易失状态接到只读诊断接口。
type adminRuntime struct {
	br  *broker.Broker
	cmd *command.Tracker
	api *httpapi.API
}

func (a adminRuntime) RecentAuth(dev string, n int) []broker.AuthEvent {
	return a.br.RecentAuth(dev, n)
}
func (a adminRuntime) ConnectedKeys(dev string) []string { return a.br.ConnectedKeys(dev) }
func (a adminRuntime) Pending(dev string) []command.PendingInfo {
	return a.cmd.PendingDetail(dev)
}
func (a adminRuntime) RegisterQuota(dev string) (int, int, time.Time) {
	return a.api.RegisterQuota(dev)
}

// adminTZ 让 CLI 的 user tz 经过运行中的进程：写库后同步重渲并通知设备。
type adminTZ struct {
	st *store.Store
	fs *frames.Service
}

func (a adminTZ) ApplyUserTZ(userID, value string) error {
	found, err := a.st.SetUserTZ(userID, value)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("用户 %s 不存在", userID)
	}
	if err := a.fs.Rebuild(userID); err != nil {
		// 渲染失败不影响设置本身；后台 worker 会再试。
		a.fs.MarkDirty(userID)
	}
	return nil
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
