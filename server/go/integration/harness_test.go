package integration

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/broker"
	"github.com/dayuer/stickbox/server/go/internal/command"
	"github.com/dayuer/stickbox/server/go/internal/config"
	"github.com/dayuer/stickbox/server/go/internal/frames"
	"github.com/dayuer/stickbox/server/go/internal/httpapi"
	"github.com/dayuer/stickbox/server/go/internal/registry"
	"github.com/dayuer/stickbox/server/go/internal/render"
	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/store"
	"github.com/dayuer/stickbox/server/go/internal/tlsx"
)

// testPepper 只用于测试进程内的令牌哈希。
const testPepper = "integration-test-pepper-0123456789"

type env struct {
	t        *testing.T
	httpBase string
	mqttAddr string
	store    *store.Store
	client   *http.Client
	cmd      *command.Tracker
}

// startServer 起一套真的：内嵌 broker + HTTPS，自签证书，端口交给内核分配。
func startServer(t *testing.T) *env {
	t.Helper()
	e, _ := startServerAt(t, t.TempDir())
	return e
}

// startServerAt 在指定目录起一套服务（同一个库可以关掉再起，模拟重启）。
// 返回的 stop 可提前调用且可重复调用；测试结束也会自动 stop。
func startServerAt(t *testing.T, dir string) (*env, func()) {
	t.Helper()
	certDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSelfSigned(t, certDir)

	cfg := &config.Config{Root: dir, CertDir: certDir}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}

	v := auth.NewVerifier(st, testPepper)
	reloader, err := tlsx.New(cfg.CertPath(), cfg.KeyPath())
	if err != nil {
		t.Fatal(err)
	}

	reg := registry.New(registry.Options{IdleTimeout: time.Hour})

	mqttAddr := freeAddr(t)
	br, err := broker.New(mqttAddr, reloader.Config(), v, reg, broker.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := br.Serve(); err != nil {
		t.Fatal(err)
	}

	api := httpapi.New(cfg, st, v, reg, br)
	br.SetUsageResender(api.Usage())
	br.SetPairResender(api)
	rr, err := render.New()
	if err != nil {
		t.Fatalf("初始化渲染器：%v", err)
	}
	fs, err := frames.New(cfg.Root, st, source.NewMostRecent(
		source.NewClaude(api.Usage(), "UTC"),
		source.NewSales(api.Sales(), "UTC"),
	), br, frames.RenderBuilder{R: rr}, frames.Options{
		Debounce:          100 * time.Millisecond, // 测试里缩短防抖
		RenderConcurrency: 1,
		BackfillPerSec:    100,
		Now:               time.Now,
	})
	if err != nil {
		t.Fatalf("初始化帧服务：%v", err)
	}
	api.SetFrames(fs)
	br.SetFrameResender(fs)

	// T18：下行指令走确认跟踪，和 main.go 的接法一致，让集成测试走真实路径。
	cmdTracker := command.New(br, reg)
	br.SetCommandTracker(cmdTracker)
	cmdDone := make(chan struct{})
	t.Cleanup(func() { close(cmdDone) })
	go cmdTracker.Run(cmdDone)
	ln, err := tls.Listen("tcp", freeAddr(t), reloader.Config())
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: api.Handler()}
	go srv.Serve(ln)

	var once sync.Once
	stop := func() {
		once.Do(func() {
			srv.Close()
			br.Close()
			fs.Close()
			reg.Shutdown()
			st.Close()
		})
	}
	t.Cleanup(stop)

	return &env{
		t: t, httpBase: "https://" + ln.Addr().String(), mqttAddr: mqttAddr,
		store: st, cmd: cmdTracker,
		client: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 仅测试
		}},
	}, stop
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func writeSelfSigned(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(dir, "fullchain.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(filepath.Join(dir, "privkey.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}

// —— HTTP 侧 ——

func (e *env) req(method, path, body string, hdr map[string]string) (int, []byte) {
	e.t.Helper()
	st, b, _ := e.reqHeaders(method, path, body, hdr)
	return st, b
}

func (e *env) reqHeaders(method, path, body string, hdr map[string]string) (int, []byte, http.Header) {
	e.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.httpBase+path, r)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, resp.Header
}

func (e *env) register(dev string) (token, code string) {
	e.t.Helper()
	st, b := e.req("POST", "/api/device/register", `{"dev":"`+dev+`"}`,
		map[string]string{"Content-Type": "application/json"})
	if st != 200 {
		e.t.Fatalf("register = %d %s", st, b)
	}
	var out struct {
		Token string `json:"token"`
		Code  string `json:"code"`
	}
	json.Unmarshal(b, &out)
	return out.Token, out.Code
}

func (e *env) tryPair(code, agentToken string) (int, []byte) {
	e.t.Helper()
	hdr := map[string]string{"Content-Type": "application/json"}
	if agentToken != "" {
		hdr["Authorization"] = "Bearer " + agentToken
	}
	return e.req("POST", "/api/agent/pair",
		fmt.Sprintf(`{"code":"%s","name":"itest"}`, code), hdr)
}

func (e *env) pair(code, agentToken string) (token, dev, userID string) {
	e.t.Helper()
	st, b := e.tryPair(code, agentToken)
	if st != 200 {
		e.t.Fatalf("pair = %d %s", st, b)
	}
	var out struct {
		Token  string `json:"token"`
		Dev    string `json:"dev"`
		UserID string `json:"user_id"`
	}
	json.Unmarshal(b, &out)
	return out.Token, out.Dev, out.UserID
}

func (e *env) reportUsage(agentToken string, body []byte) {
	e.t.Helper()
	st, b := e.req("POST", "/api/usage", string(body),
		map[string]string{"Content-Type": "application/json",
			"Authorization": "Bearer " + agentToken})
	if st != 200 {
		e.t.Fatalf("上报用量 = %d %s", st, b)
	}
}

func (e *env) reportData(agentToken, body string) {
	e.t.Helper()
	st, b := e.req("POST", "/api/data", body,
		map[string]string{"Content-Type": "application/json",
			"Authorization": "Bearer " + agentToken})
	if st != 200 {
		e.t.Fatalf("上报经营数据 = %d %s", st, b)
	}
}

func (e *env) getFrame(devKey, ifNoneMatch string) (int, []byte, string) {
	e.t.Helper()
	hdr := map[string]string{"Authorization": "Bearer " + devKey}
	if ifNoneMatch != "" {
		hdr["If-None-Match"] = ifNoneMatch
	}
	st, b, h := e.reqHeaders("GET", "/api/device/frame", "", hdr)
	return st, b, h.Get("ETag")
}

func (e *env) unpair(agentToken, dev string) {
	e.t.Helper()
	st, b := e.req("POST", "/api/agent/unpair", `{"dev":"`+dev+`"}`,
		map[string]string{"Content-Type": "application/json",
			"Authorization": "Bearer " + agentToken})
	if st != 200 {
		e.t.Fatalf("解绑 = %d %s", st, b)
	}
}

// —— 设备侧（用 paho 当假固件）——

type fakeDevice struct {
	t     *testing.T
	c     paho.Client
	dev   string
	usage chan []byte
	pair  chan []byte
	frame chan []byte
}

func (e *env) connectDevice(dev, key string) *fakeDevice {
	e.t.Helper()
	d, err := e.tryConnectDevice(dev, key)
	if err != nil {
		e.t.Fatalf("设备连不上：%v", err)
	}
	return d
}

func (e *env) tryConnectDevice(dev, key string) (*fakeDevice, error) {
	return e.tryConnectDeviceAs(dev, key, dev)
}

// tryConnectDeviceAs 用指定的 client id 连接。
//
// 为什么需要它：MQTT 的会话接管按 client id 算，同 id 的新连接会把旧的踢掉。
// 测「pending 密钥不能写 ack」时如果两边用同一个 client id，合法设备会先被
// 顶掉，测出来的是顶号而不是 ACL。顶号本身是另一个问题（已单独报），
// 这条测试要把它隔离开。
func (e *env) tryConnectDeviceAs(dev, key, clientID string) (*fakeDevice, error) {
	opts := paho.NewClientOptions().
		AddBroker("ssl://" + e.mqttAddr).
		SetClientID(clientID).
		SetUsername(dev).
		SetPassword(key).
		SetTLSConfig(&tls.Config{InsecureSkipVerify: true}).
		SetConnectTimeout(5 * time.Second).
		SetAutoReconnect(false)

	fd := &fakeDevice{t: e.t, dev: dev,
		usage: make(chan []byte, 8), pair: make(chan []byte, 8),
		frame: make(chan []byte, 8)}
	opts.SetDefaultPublishHandler(func(_ paho.Client, m paho.Message) {
		payload := append([]byte(nil), m.Payload()...)
		switch {
		case strings.HasSuffix(m.Topic(), "/usage"):
			select {
			case fd.usage <- payload:
			default:
			}
		case strings.HasSuffix(m.Topic(), "/pair"):
			select {
			case fd.pair <- payload:
			default:
			}
		case strings.HasSuffix(m.Topic(), "/frame"):
			select {
			case fd.frame <- payload:
			default:
			}
		}
	})
	c := paho.NewClient(opts)
	tok := c.Connect()
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		return nil, fmt.Errorf("连接失败：%v", tok.Error())
	}
	fd.c = c
	e.t.Cleanup(func() { c.Disconnect(100) })
	return fd, nil
}

// legacyDevice 模拟旧固件：MQTT_EVENT_CONNECTED 时无条件订阅 usage+pair，
// 断线后自动重连并重订。用来验证服务端踢连接后旧固件能恢复。
type legacyDevice struct {
	t          *testing.T
	c          paho.Client
	dev        string
	usage      chan []byte
	subscribed chan struct{}
}

func (e *env) connectLegacyDevice(dev, key string) (*legacyDevice, error) {
	d := &legacyDevice{t: e.t, dev: dev,
		usage: make(chan []byte, 8), subscribed: make(chan struct{}, 8)}
	opts := paho.NewClientOptions().
		AddBroker("ssl://" + e.mqttAddr).
		SetClientID(dev).
		SetUsername(dev).
		SetPassword(key).
		SetTLSConfig(&tls.Config{InsecureSkipVerify: true}).
		SetConnectTimeout(5 * time.Second).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetMaxReconnectInterval(time.Second).
		SetCleanSession(true)
	opts.SetDefaultPublishHandler(func(_ paho.Client, m paho.Message) {
		if !strings.HasSuffix(m.Topic(), "/usage") {
			return
		}
		select {
		case d.usage <- append([]byte(nil), m.Payload()...):
		default:
		}
	})
	opts.SetOnConnectHandler(func(c paho.Client) {
		c.Subscribe("printer/"+dev+"/pair", 1, nil)
		tok := c.Subscribe("printer/"+dev+"/usage", 1, nil)
		go func() {
			if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
				return
			}
			if st, ok := tok.(*paho.SubscribeToken); ok {
				for _, code := range st.Result() {
					if code >= 0x80 {
						return // 还没配对，usage 被拒
					}
				}
			}
			select {
			case d.subscribed <- struct{}{}:
			default:
			}
		}()
	})
	c := paho.NewClient(opts)
	tok := c.Connect()
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		return nil, fmt.Errorf("连接失败：%v", tok.Error())
	}
	d.c = c
	e.t.Cleanup(func() { c.Disconnect(100) })
	return d, nil
}

func (d *legacyDevice) waitUsage(timeout time.Duration) []byte {
	d.t.Helper()
	select {
	case b := <-d.usage:
		return b
	case <-time.After(timeout):
		d.t.Fatal("旧固件没有收到用量")
		return nil
	}
}

// subscribe 必须检查 SUBACK 的返回码：broker 拒绝订阅时回 0x80，
// 而 paho 的 token.Error() 是 nil。
func (d *fakeDevice) subscribe(topic string) error {
	tok := d.c.Subscribe(topic, 1, nil)
	if !tok.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("订阅 %s 超时", topic)
	}
	if err := tok.Error(); err != nil {
		return err
	}
	st, ok := tok.(*paho.SubscribeToken)
	if !ok {
		return nil
	}
	for f, code := range st.Result() {
		if code >= 0x80 {
			return fmt.Errorf("订阅 %s 被拒（SUBACK 0x%02x）", f, code)
		}
	}
	return nil
}

func (d *fakeDevice) subscribeUsage() error {
	return d.subscribe("printer/" + d.dev + "/usage")
}
func (d *fakeDevice) subscribePair() error {
	return d.subscribe("printer/" + d.dev + "/pair")
}
func (d *fakeDevice) subscribeFrame() error {
	return d.subscribe("printer/" + d.dev + "/frame")
}

func (d *fakeDevice) heartbeat() {
	d.t.Helper()
	payload, _ := json.Marshal(map[string]any{"dev": d.dev, "state": "online"})
	tok := d.c.Publish("printer/"+d.dev+"/status", 1, false, payload)
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		d.t.Fatalf("心跳发送失败：%v", tok.Error())
	}
}

func (d *fakeDevice) disconnect() { d.c.Disconnect(100) }

// publishAck 发回执并返回 publish 的错误（ACL 拒绝时用得上）。
func (d *fakeDevice) publishAck(cid string, ok bool, errMsg string) error {
	n := 0
	if ok {
		n = 1
	}
	p := fmt.Sprintf(`{"cid":%q,"ok":%d}`, cid, n)
	if errMsg != "" {
		p = fmt.Sprintf(`{"cid":%q,"ok":%d,"e":%q}`, cid, n, errMsg)
	}
	tk := d.c.Publish("printer/"+d.dev+"/ack", 1, false, p)
	if !tk.WaitTimeout(3 * time.Second) {
		return fmt.Errorf("publish 超时")
	}
	return tk.Error()
}

// ack 回一条指令回执，模拟设备「应用完成」。
func (d *fakeDevice) ack(cid string, ok bool, errMsg string) {
	n := 0
	if ok {
		n = 1
	}
	p := fmt.Sprintf(`{"cid":%q,"ok":%d}`, cid, n)
	if errMsg != "" {
		p = fmt.Sprintf(`{"cid":%q,"ok":%d,"e":%q}`, cid, n, errMsg)
	}
	tk := d.c.Publish("printer/"+d.dev+"/ack", 1, false, p)
	tk.WaitTimeout(3 * time.Second)
}

// cidOf 取出下行消息里的 cid。没有 cid 的指令设备无从确认。
func cidOf(t *testing.T, payload []byte) string {
	t.Helper()
	var m struct {
		CID string `json:"cid"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("下行不是 JSON：%s", payload)
	}
	if m.CID == "" {
		t.Fatalf("下行缺 cid，设备无从回执：%s", payload)
	}
	return m.CID
}

func (d *fakeDevice) waitUsage(timeout time.Duration) []byte {
	d.t.Helper()
	select {
	case b := <-d.usage:
		return b
	case <-time.After(timeout):
		d.t.Fatal("没有收到用量")
		return nil
	}
}

func (d *fakeDevice) waitPair(timeout time.Duration) []byte {
	d.t.Helper()
	select {
	case b := <-d.pair:
		return b
	case <-time.After(timeout):
		d.t.Fatal("没有收到配对通知")
		return nil
	}
}

func (d *fakeDevice) waitFrameVer(timeout time.Duration) uint64 {
	d.t.Helper()
	select {
	case b := <-d.frame:
		var p struct {
			Ver uint64 `json:"ver"`
		}
		if err := json.Unmarshal(b, &p); err != nil || p.Ver == 0 {
			d.t.Fatalf("帧版本通知 = %s", b)
		}
		return p.Ver
	case <-time.After(timeout):
		d.t.Fatal("没有收到帧版本通知")
		return 0
	}
}
