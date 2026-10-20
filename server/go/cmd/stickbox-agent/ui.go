package main

// stickbox-agent ui：只在本机（127.0.0.1）起的小网页，用来输入电视上的 6 位配对码、
// 管理设备。不是远程管理界面：
//   - 只绑定 127.0.0.1，且拒绝非本机 Host 头（防 DNS 重绑定）；
//   - 每次启动随机 token，放在 URL 的 ?t= 里，API 要求 X-Stickbox-Token 头；
//   - POST 另校验 Origin 与 Host 一致（防 CSRF）；
//   - 页面零外部资源，CSP 全部限制为同源；
//   - 响应和日志里绝不出现 agent 令牌；
//   - 空闲 15 分钟或配对成功 60 秒后自动退出。

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed ui_assets
var uiAssets embed.FS

const (
	uiIdleTimeout    = 15 * time.Minute
	uiAfterPairExit  = 60 * time.Second
	uiMaxBody        = 4 << 10
	uiPairFailLimit  = 5 // 每个窗口内最多失败次数
	uiPairFailWindow = time.Minute
)

type uiOptions struct {
	server   string
	name     string
	insecure bool
}

type uiServer struct {
	opts  uiOptions
	token string
	port  int

	idle      time.Duration
	afterPair time.Duration
	now       func() time.Time
	logf      func(string, ...any)
	// runRound 默认是 runRound；单测里换成假的，避免真的拉起 claude。
	runRound func(*agentConfig, func(string, ...any)) error

	// 局域网发现与读码，可注入（单测用假板子）。
	disc  discoverer
	codes codeReader

	discMu  sync.Mutex // 发现不重叠
	foundMu sync.Mutex
	found   map[string]foundDevice // 最近一次发现的设备，配对只认这里的 IP，不信页面传来的地址

	mu    sync.Mutex // 串行化会改配置/服务端状态的操作（pair / unpair）
	runMu sync.Mutex // run-once 不重叠

	failMu   sync.Mutex
	failures []time.Time

	activity chan struct{}
	paired   chan struct{}
	pairOnce sync.Once

	secretMu sync.Mutex
	secrets  []string // 本进程见过的令牌，出站前从响应体里抹掉（纵深防御）
}

func newUIServer(opts uiOptions) (*uiServer, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("生成 token 失败：%w", err)
	}
	return &uiServer{
		opts:      opts,
		token:     hex.EncodeToString(b[:]),
		idle:      uiIdleTimeout,
		afterPair: uiAfterPairExit,
		now:       time.Now,
		logf:      func(f string, a ...any) { log.Printf(f, a...) },
		runRound:  runRound,
		disc:      newUDPDiscoverer(),
		codes:     newHTTPCodeReader(),
		found:     map[string]foundDevice{},
		activity:  make(chan struct{}, 1),
		paired:    make(chan struct{}),
	}, nil
}

// ---- 安全层 ----

// hostAllowed：Host 头只认本机回环名 + 实际监听端口。
func (s *uiServer) hostAllowed(host string) bool {
	host = strings.ToLower(host)
	p := fmt.Sprint(s.port)
	return host == "127.0.0.1:"+p || host == "localhost:"+p
}

// originOK：没有 Origin 头时（GET、curl）放行；有就必须与 Host 完全一致。
// POST 必须带 Origin（浏览器的 fetch POST 总会带）。
func originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return r.Method != http.MethodPost
	}
	return o == "http://"+r.Host
}

func (s *uiServer) tokenOK(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *uiServer) touch() {
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

func setSecurityHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer") // URL 里带着 token
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

// guard 包住所有路由：Host → Origin → 记活动。token 在各路由里按需要校验。
func (s *uiServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		if !s.hostAllowed(r.Host) {
			http.Error(w, "forbidden: host", http.StatusForbidden)
			return
		}
		if !originOK(r) {
			http.Error(w, "forbidden: origin", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, uiMaxBody)
		s.touch()
		next.ServeHTTP(w, r)
	})
}

// ---- 输出：统一抹掉令牌 ----

func (s *uiServer) addSecret(tok string) {
	if tok == "" {
		return
	}
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	for _, x := range s.secrets {
		if x == tok {
			return
		}
	}
	s.secrets = append(s.secrets, tok)
}

// scrub 把响应体里出现的任何已知 agent 令牌换成 ***。
// 正常路径里令牌根本不会进入响应；这是兜底（例如错误文本里意外带出）。
func (s *uiServer) scrub(b []byte) []byte {
	if c, err := loadConfig(); err == nil {
		s.addSecret(c.Token)
	}
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	out := string(b)
	for _, t := range s.secrets {
		out = strings.ReplaceAll(out, t, "***")
	}
	return []byte(out)
}

func (s *uiServer) writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(s.scrub(b))
}

// ---- 路由 ----

func (s *uiServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/app.js", s.staticHandler("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("/app.css", s.staticHandler("app.css", "text/css; charset=utf-8"))
	s.apiRoutes(mux)
	return s.guard(mux)
}

func (s *uiServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.tokenOK(r.URL.Query().Get("t")) {
		http.Error(w, "unauthorized：请用 stickbox-agent ui 打印的完整地址打开（带 ?t=…）", http.StatusUnauthorized)
		return
	}
	s.serveAsset(w, r, "index.html", "text/html; charset=utf-8")
}

func (s *uiServer) staticHandler(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveAsset(w, r, name, ctype)
	}
}

func (s *uiServer) serveAsset(w http.ResponseWriter, r *http.Request, name, ctype string) {
	b, err := uiAssets.ReadFile("ui_assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(b)
}

// ---- 生命周期 ----

func (s *uiServer) markPaired() {
	s.pairOnce.Do(func() { close(s.paired) })
}

// serve 在 ln 上提供服务，直到 ctx 取消、空闲超时或配对成功后 afterPair 到期。
// 返回退出原因。
func (s *uiServer) serve(ctx context.Context, ln net.Listener) (string, error) {
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	reason := ""
	idleT := time.NewTimer(s.idle)
	defer idleT.Stop()
	var pairT <-chan time.Time
	paired := s.paired
loop:
	for {
		select {
		case <-ctx.Done():
			reason = "收到退出信号"
			break loop
		case <-s.activity:
			if pairT == nil {
				if !idleT.Stop() {
					select {
					case <-idleT.C:
					default:
					}
				}
				idleT.Reset(s.idle)
			}
		case <-idleT.C:
			if pairT == nil {
				reason = fmt.Sprintf("空闲 %s，自动退出", s.idle)
				break loop
			}
		case <-paired:
			paired = nil
			pairT = time.After(s.afterPair)
		case <-pairT:
			reason = "配对已完成，自动退出"
			break loop
		case err := <-errc:
			return "", err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return reason, nil
}

// listenLoopback 只监听 127.0.0.1。port=0 取随机空闲端口。
func listenLoopback(port int) (net.Listener, error) {
	return net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
}

// openBrowser 用系统默认浏览器打开 URL；单测里替换。
var openBrowser = func(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func cmdUI(args []string) error {
	fs := newFlagSet("ui")
	port := fs.Int("port", 0, "监听端口（默认随机空闲端口；只绑定 127.0.0.1）")
	noOpen := fs.Bool("no-open", false, "只打印地址，不自动打开浏览器")
	server := fs.String("server", "", "服务端地址（默认 "+defaultServer+"）")
	name := fs.String("name", "", "这台电脑的名称（默认主机名）")
	insecure := fs.Bool("insecure", false, "跳过 TLS 证书校验（仅自签名证书时使用）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("用法：stickbox-agent ui [--port N] [--no-open] [--server URL] [--name 名称] [--insecure]")
	}
	if *port < 0 || *port > 65535 {
		return errors.New("端口应在 0–65535 之间")
	}
	return runUICommand(uiOptions{server: *server, name: *name, insecure: *insecure}, !*noOpen, *port)
}

func runUICommand(opts uiOptions, open bool, port int) error {
	s, err := newUIServer(opts)
	if err != nil {
		return err
	}
	ln, err := listenLoopback(port)
	if err != nil {
		return fmt.Errorf("监听 127.0.0.1 失败：%w", err)
	}
	defer ln.Close()
	s.port = ln.Addr().(*net.TCPAddr).Port

	u := (&url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", s.port), Path: "/", RawQuery: "t=" + s.token}).String()
	fmt.Println("配对网页已启动（只在本机可访问）：")
	fmt.Println("  " + u)
	fmt.Printf("闲置 %d 分钟或配对成功后 %d 秒自动退出，Ctrl-C 随时退出。\n", int(s.idle.Minutes()), int(s.afterPair.Seconds()))
	if open {
		if err := openBrowser(u); err != nil {
			fmt.Fprintf(os.Stderr, "没能自动打开浏览器（%v），请手动打开上面的地址\n", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reason, err := s.serve(ctx, ln)
	if err != nil {
		return err
	}
	fmt.Println(reason)
	return nil
}
