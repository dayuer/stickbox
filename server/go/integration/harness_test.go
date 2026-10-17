package integration

import (
	"context"
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
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/broker"
	"github.com/dayuer/stickbox/server/go/internal/config"
	"github.com/dayuer/stickbox/server/go/internal/httpapi"
	"github.com/dayuer/stickbox/server/go/internal/registry"
	"github.com/dayuer/stickbox/server/go/internal/store"
	"github.com/dayuer/stickbox/server/go/internal/tlsx"
)

type fakeSender struct{ last string }

func (f *fakeSender) Send(ctx context.Context, phone, code string) error {
	f.last = code
	return nil
}

type env struct {
	t        *testing.T
	httpBase string
	mqttAddr string
	store    *store.Store
	sender   *fakeSender
	client   *http.Client
}

// startServer 起一套真的：内嵌 broker + HTTPS，自签证书，端口交给内核分配。
// 固定端口会让并行测试互相打架。
func startServer(t *testing.T) *env {
	t.Helper()
	return startServerWithDevLogin(t, "", "")
}

// startServerWithDevLogin 同上，另开固定手机号旁路（devPhone 为空则不开）。
func startServerWithDevLogin(t *testing.T, devPhone, devCode string) *env {
	t.Helper()
	dir := t.TempDir()
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
	t.Cleanup(func() { st.Close() })

	pb, err := auth.NewPhoneBox("itest-pepper",
		"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	v := auth.NewVerifier(st)
	smsSvc := auth.NewSMS(st, sender, time.Now)
	if devPhone != "" {
		smsSvc.SetDevLogin(pb.HMAC(devPhone), devCode)
	}
	mem := broker.NewMembership(st)
	reloader, err := tlsx.New(cfg.CertPath(), cfg.KeyPath())
	if err != nil {
		t.Fatal(err)
	}

	reg := registry.New(registry.Options{IdleTimeout: time.Hour})
	t.Cleanup(reg.Shutdown)

	mqttAddr := freeAddr(t)
	br, err := broker.New(mqttAddr, reloader.Config(), v, mem, reg, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := br.Serve(); err != nil { // 非阻塞：启动监听后立即返回
		t.Fatal(err)
	}
	t.Cleanup(func() { br.Close() })

	api := httpapi.New(cfg, st, v, pb, smsSvc, reg, mem, br)
	br.SetUsageResender(api.Usage())
	ln, err := tls.Listen("tcp", freeAddr(t), reloader.Config())
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: api.Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	return &env{
		t: t, httpBase: "https://" + ln.Addr().String(), mqttAddr: mqttAddr,
		store: st, sender: sender,
		client: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 仅测试
		}},
	}
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
	return resp.StatusCode, out
}

func (e *env) sendCode(phone string) string {
	e.t.Helper()
	if code, b := e.req("POST", "/api/auth/sms", `{"phone":"`+phone+`"}`,
		map[string]string{"Content-Type": "application/json"}); code != 200 {
		e.t.Fatalf("发码 = %d %s", code, b)
	}
	return e.sender.last
}

func (e *env) login(phone string) string {
	e.t.Helper()
	return e.verify(phone, e.sendCode(phone))
}

// loginWithCode 用已知验证码登录，给固定手机号用。
func (e *env) loginWithCode(phone, code string) string {
	e.t.Helper()
	e.sendCode(phone)
	return e.verify(phone, code)
}

func (e *env) verify(phone, verifyCode string) string {
	e.t.Helper()
	code, b := e.req("POST", "/api/auth/verify",
		fmt.Sprintf(`{"phone":"%s","code":"%s","device":"itest"}`, phone, verifyCode),
		map[string]string{"Content-Type": "application/json"})
	if code != 200 {
		e.t.Fatalf("登录 = %d %s", code, b)
	}
	var out struct {
		Token string `json:"token"`
	}
	json.Unmarshal(b, &out)
	return out.Token
}

// agentLogin 为已有账号签发一把 agent 令牌。
func (e *env) agentLogin(phone string) string {
	e.t.Helper()
	return e.agentLoginWithCode(phone, e.sendCode(phone))
}

// agentLoginWithCode 不再发码：调用方已经让 sms_codes 里有了这条记录。
// 固定手机号旁路下，先调 sendCode 写入固定验证码，再传固定 code 即可。
func (e *env) agentLoginWithCode(phone, verifyCode string) string {
	e.t.Helper()
	code, b := e.req("POST", "/api/agent/login",
		fmt.Sprintf(`{"phone":"%s","code":"%s","name":"itest-mac"}`, phone, verifyCode),
		map[string]string{"Content-Type": "application/json"})
	if code != 200 {
		e.t.Fatalf("agent 登录 = %d %s", code, b)
	}
	var out struct {
		Token string `json:"token"`
	}
	json.Unmarshal(b, &out)
	return out.Token
}

func (e *env) enroll(token, dev string) string {
	e.t.Helper()
	code, b := e.req("POST", "/api/device/enroll", `{"dev":"`+dev+`"}`,
		map[string]string{"Content-Type": "application/json",
			"Authorization": "Bearer " + token})
	if code != 200 {
		e.t.Fatalf("enroll = %d %s", code, b)
	}
	var out struct {
		DeviceKey string `json:"device_key"`
	}
	json.Unmarshal(b, &out)
	return out.DeviceKey
}

func (e *env) reportUsage(agentToken string, body []byte) {
	e.t.Helper()
	code, b := e.req("POST", "/api/usage", string(body),
		map[string]string{"Content-Type": "application/json",
			"Authorization": "Bearer " + agentToken})
	if code != 200 {
		e.t.Fatalf("上报用量 = %d %s", code, b)
	}
}

// —— 设备侧（用 paho 当假固件）——

type fakeDevice struct {
	t     *testing.T
	c     paho.Client
	dev   string
	usage chan []byte
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
	opts := paho.NewClientOptions().
		AddBroker("ssl://" + e.mqttAddr).
		SetClientID(dev).
		SetUsername(dev).
		SetPassword(key).
		SetTLSConfig(&tls.Config{InsecureSkipVerify: true}).
		SetConnectTimeout(5 * time.Second).
		SetAutoReconnect(false)

	fd := &fakeDevice{t: e.t, dev: dev, usage: make(chan []byte, 8)}
	opts.SetDefaultPublishHandler(func(_ paho.Client, m paho.Message) {
		if strings.HasSuffix(m.Topic(), "/usage") {
			payload := append([]byte(nil), m.Payload()...)
			select {
			case fd.usage <- payload:
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

// subscribe 必须检查 SUBACK 的返回码：broker 拒绝订阅时回 0x80，
// 而 paho 的 token.Error() 是 nil——只看 error 会以为订阅成功了。
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

func (d *fakeDevice) disconnect() {
	d.c.Disconnect(100)
}

func (d *fakeDevice) heartbeat(state string) {
	d.t.Helper()
	payload, _ := json.Marshal(map[string]any{"dev": d.dev, "state": state})
	tok := d.c.Publish("printer/"+d.dev+"/status", 1, false, payload)
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		d.t.Fatalf("心跳发送失败：%v", tok.Error())
	}
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
