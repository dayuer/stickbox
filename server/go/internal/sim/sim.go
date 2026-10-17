// Package sim 是一台假设备：走真设备的注册、MQTT、取帧与拼接流程。
package sim

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

const version = "0.1.0"

var (
	errNoToken   = errors.New("sim: 没有设备令牌")
	errNeedRereg = errors.New("sim: 需要重新注册")
)

type Config struct {
	Server     string // https://mqtt.stickbox.ai
	MQTTAddr   string // host:port；留空从 Server 推 :8883
	StateDir   string // 密钥/配对码持久化目录
	Insecure   bool
	StreamAddr string // MJPEG/AVI 监听地址；留空不启流
	TV         string // 可选：DLNA 电视 IP/主机名（--push-tv）；留空只起流不推送
	FPS        int
	Burst      int
	Dev        string // 可选：固定设备 ID（默认随机生成并持久化）
	Logf       func(string, ...any)
}

type state struct {
	Server      string `json:"server"`
	MQTTAddr    string `json:"mqtt_addr,omitempty"`
	Dev         string `json:"dev"`
	Token       string `json:"token,omitempty"`
	Code        string `json:"code,omitempty"`
	CodeExpires int64  `json:"code_expires,omitempty"`
	Paired      bool   `json:"paired,omitempty"`
}

type Device struct {
	cfg Config
	hc  *http.Client

	regMu   sync.Mutex
	fetchMu sync.Mutex

	mu             sync.RWMutex
	st             state
	bundle         *Bundle
	etag           string
	jpeg           []byte
	lastMinute     string
	needRegister   bool
	dataSubscribed bool // 当前连接是否已订阅 usage/frame（配对后才订）

	frameReady chan struct{}
	stream     *streamServer
}

func New(cfg Config) (*Device, error) {
	if cfg.Server == "" {
		cfg.Server = "https://mqtt.stickbox.ai"
	}
	cfg.Server = strings.TrimRight(cfg.Server, "/")
	if _, err := url.Parse(cfg.Server); err != nil {
		return nil, fmt.Errorf("sim: 服务端地址不合法：%w", err)
	}
	if cfg.MQTTAddr == "" {
		u, _ := url.Parse(cfg.Server)
		host := u.Hostname()
		if host == "" {
			return nil, errors.New("sim: 无法从服务端地址推断 MQTT 地址")
		}
		cfg.MQTTAddr = host + ":8883"
	}
	cfg.MQTTAddr = strings.TrimPrefix(strings.TrimPrefix(cfg.MQTTAddr, "ssl://"), "mqtts://")
	if cfg.StateDir == "" {
		cfg.StateDir = "stickbox-sim-state"
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 2
	}
	if cfg.Burst < 0 {
		cfg.Burst = 0
	} else if cfg.Burst == 0 {
		cfg.Burst = 8
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("sim: 建状态目录：%w", err)
	}

	d := &Device{
		cfg:        cfg,
		frameReady: make(chan struct{}, 1),
		hc: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.Insecure},
			},
		},
	}
	d.load()
	if cfg.Dev != "" {
		d.mu.Lock()
		if d.st.Dev != cfg.Dev {
			d.st.Dev = cfg.Dev
			d.st.Token = ""
			d.st.Code = ""
			d.st.Paired = false
		}
		d.mu.Unlock()
	}
	d.mu.Lock()
	if d.st.Server != "" && d.st.Server != cfg.Server {
		// 换服务端了，旧令牌不能复用。
		d.st.Token = ""
		d.st.Code = ""
		d.st.Paired = false
	}
	d.st.Server = cfg.Server
	d.st.MQTTAddr = cfg.MQTTAddr
	if d.st.Dev == "" {
		d.st.Dev = randomDev()
	}
	d.mu.Unlock()
	if err := d.save(); err != nil {
		return nil, err
	}
	d.setWaiting()
	return d, nil
}

func randomDev() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (d *Device) statePath() string { return filepath.Join(d.cfg.StateDir, "device.json") }

func (d *Device) load() {
	raw, err := os.ReadFile(d.statePath())
	if err != nil {
		return
	}
	var st state
	if json.Unmarshal(raw, &st) == nil {
		d.st = st
	}
}

func (d *Device) save() error {
	d.mu.RLock()
	st := d.st
	d.mu.RUnlock()
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(d.cfg.StateDir, ".device-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, d.statePath())
}

func (d *Device) logf(format string, a ...any) { d.cfg.Logf(format, a...) }

func (d *Device) Dev() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.st.Dev
}

func (d *Device) Code() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.st.Code
}

func (d *Device) FrameReady() <-chan struct{} { return d.frameReady }

func (d *Device) signalFrame() {
	select {
	case d.frameReady <- struct{}{}:
	default:
	}
}

// CurrentJPEG 返回当前要推给电视的整帧（等待画面或拼接帧）。
func (d *Device) CurrentJPEG() []byte {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.jpeg
}

func (d *Device) Bundle() *Bundle {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.bundle
}

func (d *Device) setWaiting() {
	d.mu.RLock()
	code, paired, hasBundle := d.st.Code, d.st.Paired, d.bundle != nil
	d.mu.RUnlock()
	if hasBundle {
		return
	}
	jpg, err := waitingJPEG(code, paired)
	if err != nil {
		d.logf("生成等待画面失败：%v", err)
		return
	}
	d.mu.Lock()
	d.jpeg = jpg
	d.mu.Unlock()
}

// —— HTTP ——

func (d *Device) api(ctx context.Context, method, path string, in, out any, token string) (int, http.Header, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.cfg.Server+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", "stickbox-sim/"+version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, resp.Header, err
		}
	}
	return resp.StatusCode, resp.Header, nil
}

type registerResp struct {
	Token     string `json:"token"`
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expires_at"`
	Now       int64  `json:"now"`
	Pending   bool   `json:"pending"`
}

// Register 首次注册（或复用已保存的 pending 密钥换码），返回配对码。
// 已经配对时返回空码。
func (d *Device) Register(ctx context.Context) (string, error) {
	d.regMu.Lock()
	defer d.regMu.Unlock()

	d.mu.RLock()
	st := d.st
	d.mu.RUnlock()

	if st.Token != "" {
		var out struct {
			Code      string `json:"code"`
			ExpiresAt int64  `json:"expires_at"`
			Now       int64  `json:"now"`
		}
		status, _, err := d.api(ctx, http.MethodPost, "/api/device/pair-code", map[string]any{}, &out, st.Token)
		switch {
		case err != nil:
			return "", err
		case status == http.StatusOK:
			d.mu.Lock()
			d.st.Code, d.st.CodeExpires, d.st.Paired = out.Code, out.ExpiresAt, false
			d.mu.Unlock()
			_ = d.save()
			d.setWaiting()
			d.logf("设备 %s：新的配对码 %s", st.Dev, out.Code)
			return out.Code, nil
		case status == http.StatusConflict:
			d.mu.Lock()
			d.st.Paired = true
			d.st.Code = ""
			d.mu.Unlock()
			_ = d.save()
			d.setWaiting()
			return "", nil
		case status == http.StatusUnauthorized:
			d.mu.Lock()
			d.st.Token, d.st.Code, d.st.Paired = "", "", false
			d.mu.Unlock()
		default:
			return "", fmt.Errorf("sim: pair-code 返回 %d", status)
		}
	}

	var out registerResp
	status, _, err := d.api(ctx, http.MethodPost, "/api/device/register",
		map[string]string{"dev": st.Dev}, &out, "")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK || out.Token == "" || len(out.Code) != 6 {
		return "", fmt.Errorf("sim: register 返回 %d", status)
	}
	d.mu.Lock()
	d.st.Token = out.Token
	d.st.Code = out.Code
	d.st.CodeExpires = out.ExpiresAt
	d.st.Paired = false
	d.mu.Unlock()
	if err := d.save(); err != nil {
		return "", err
	}
	d.setWaiting()
	d.logf("设备 %s 已注册，配对码 %s（%d 秒有效）", st.Dev, out.Code, out.ExpiresAt-out.Now)
	d.logf("在电脑上运行：stickbox-agent pair %s", out.Code)
	return out.Code, nil
}

// FetchFrame 拉当前帧包。304 时保持现有帧；解析/校验失败也保持旧帧。
func (d *Device) FetchFrame(ctx context.Context) error {
	d.fetchMu.Lock()
	defer d.fetchMu.Unlock()

	d.mu.RLock()
	token, etag := d.st.Token, d.etag
	d.mu.RUnlock()
	if token == "" {
		return errNoToken
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.cfg.Server+"/api/device/frame", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "stickbox-sim/"+version)
	req.Header.Set("Authorization", "Bearer "+token)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil
	case http.StatusUnauthorized:
		d.mu.Lock()
		d.st.Token = ""
		d.needRegister = true
		d.mu.Unlock()
		return errNeedRereg
	case http.StatusForbidden:
		d.logf("取帧 403：设备还没配对")
		return nil
	case http.StatusNotFound:
		d.logf("取帧 404：服务端还没有帧，等下一次重建")
		return nil
	case http.StatusOK:
	default:
		return fmt.Errorf("sim: 取帧返回 %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	b, err := ParsePack(raw)
	if err != nil {
		d.logf("拒收帧包：%v", err)
		return err
	}
	d.mu.Lock()
	d.bundle = b
	d.etag = resp.Header.Get("ETag")
	d.mu.Unlock()
	d.logf("收到帧包：%d 字节，max_band=%d，ETag=%s",
		len(raw), b.Meta.Band.MaxBandBytes, resp.Header.Get("ETag"))

	now := time.Now()
	jpg, err := Splice(b, now.Hour(), now.Minute())
	if err != nil {
		d.logf("拼接失败：%v", err)
		return err
	}
	d.mu.Lock()
	d.jpeg = jpg
	d.lastMinute = now.Format("15:04")
	d.mu.Unlock()
	d.signalFrame()
	return nil
}

func (d *Device) spliceNow() {
	now := time.Now()
	d.mu.RLock()
	b := d.bundle
	d.mu.RUnlock()
	if b == nil {
		return
	}
	jpg, err := Splice(b, now.Hour(), now.Minute())
	if err != nil {
		d.logf("拼接 %s 失败：%v", now.Format("15:04"), err)
		return
	}
	d.mu.Lock()
	d.jpeg = jpg
	d.lastMinute = now.Format("15:04")
	d.mu.Unlock()
}

func (d *Device) runClock(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			hm := now.Format("15:04")
			d.mu.RLock()
			hasBundle := d.bundle != nil
			last := d.lastMinute
			d.mu.RUnlock()
			if hasBundle && hm != last {
				d.spliceNow()
			}
		}
	}
}

// —— MQTT ——

func (d *Device) Run(ctx context.Context) error {
	d.mu.RLock()
	token := d.st.Token
	d.mu.RUnlock()
	if token == "" {
		if _, err := d.Register(ctx); err != nil {
			return err
		}
	}

	if d.cfg.StreamAddr != "" {
		s := newStreamServer(d.cfg.StreamAddr, d.cfg.FPS, d.cfg.Burst, d.CurrentJPEG)
		if err := s.Start(); err != nil {
			return err
		}
		d.stream = s
		d.logf("MJPEG/AVI 流：http://%s/live.avi（%d fps，burst %d）", s.Addr(), d.cfg.FPS, d.cfg.Burst)
		defer s.Close()
		if d.cfg.TV != "" {
			go d.pushToTV(ctx)
		}
	}

	go d.runClock(ctx)

	for {
		err := d.mqttSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		d.logf("MQTT 断开：%v；3 秒后重连", err)

		d.mu.RLock()
		need := d.needRegister
		d.mu.RUnlock()
		if need {
			d.mu.Lock()
			d.needRegister = false
			d.mu.Unlock()
			if _, err := d.Register(ctx); err != nil {
				d.logf("重新注册失败：%v", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

func (d *Device) mqttSession(ctx context.Context) error {
	d.mu.RLock()
	st := d.st
	d.mu.RUnlock()
	if st.Token == "" {
		return errNeedRereg
	}

	opts := paho.NewClientOptions().
		AddBroker("ssl://" + d.cfg.MQTTAddr).
		SetClientID(st.Dev).
		SetUsername(st.Dev).
		SetPassword(st.Token).
		SetTLSConfig(&tls.Config{InsecureSkipVerify: d.cfg.Insecure}).
		SetConnectTimeout(10 * time.Second).
		SetKeepAlive(60 * time.Second).
		SetPingTimeout(10 * time.Second).
		SetAutoReconnect(false).
		SetCleanSession(true)

	lost := make(chan struct{}, 1)
	kick := make(chan struct{}, 1)
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		d.logf("MQTT 连接丢失：%v", err)
		select {
		case lost <- struct{}{}:
		default:
		}
	})
	opts.SetDefaultPublishHandler(func(c paho.Client, m paho.Message) {
		d.onMessage(ctx, c, kick, m.Topic(), m.Payload())
	})

	c := paho.NewClient(opts)
	d.logf("MQTT 连接中：%s（client id %s）", d.cfg.MQTTAddr, st.Dev)
	tok := c.Connect()
	if !tok.WaitTimeout(10 * time.Second) {
		return errors.New("sim: MQTT 连接超时")
	}
	if err := tok.Error(); err != nil {
		d.logf("MQTT 连接失败：%v", err)
		return err
	}
	d.logf("MQTT 已连接")
	defer c.Disconnect(100)

	// 新连接：只订 pair。usage/frame 属于配对后才订阅的主题；
	// 已配对时 retained 的 paired=true 会触发 subscribeData。
	d.mu.Lock()
	d.dataSubscribed = false
	d.mu.Unlock()
	pairTopic := "printer/" + st.Dev + "/pair"
	pt := c.Subscribe(pairTopic, 1, nil)
	if !pt.WaitTimeout(5*time.Second) || pt.Error() != nil {
		return fmt.Errorf("sim: 订阅 %s 失败：%v", pairTopic, pt.Error())
	}
	if sub, ok := pt.(*paho.SubscribeToken); ok {
		for f, code := range sub.Result() {
			if code >= 0x80 {
				return fmt.Errorf("sim: 订阅 %s 被拒 0x%02x", f, code)
			}
		}
	}

	select {
	case <-ctx.Done():
		return nil
	case <-lost:
		d.logf("MQTT 会话结束：连接丢失")
		return errors.New("sim: MQTT 连接断开")
	case <-kick:
		// 配对刚转正，旧连接的身份还是 pending；主动重连拿 active 身份。
		return errors.New("sim: 数据订阅被拒，主动重连")
	case <-time.After(24 * time.Hour):
		return errors.New("sim: MQTT 会话到期")
	}
}

// subscribeData 在配对成功后订阅 usage/frame。服务端配对时会踢一次连接：
// 如果这次订阅还拿着旧的 pending 身份，会被 SUBACK 0x80 拒绝；重连后
// retained 的 paired=true 会再来一次，届时身份已是 active。
func (d *Device) subscribeData(c paho.Client, kick chan<- struct{}) {
	defer func() {
		// 订阅失败时让会话重连；即使服务端没踢，也能自己恢复。
		d.mu.RLock()
		failed := !d.dataSubscribed
		d.mu.RUnlock()
		if failed {
			select {
			case kick <- struct{}{}:
			default:
			}
		}
	}()

	d.mu.Lock()
	if d.dataSubscribed {
		d.mu.Unlock()
		return
	}
	d.dataSubscribed = true
	dev := d.st.Dev
	d.mu.Unlock()

	for _, leaf := range []string{"usage", "frame"} {
		topic := fmt.Sprintf("printer/%s/%s", dev, leaf)
		tok := c.Subscribe(topic, 1, nil)
		if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
			d.logf("订阅 %s 失败：%v", topic, tok.Error())
			d.resetDataSubscriptions()
			continue
		}
		if sub, ok := tok.(*paho.SubscribeToken); ok {
			for f, code := range sub.Result() {
				if code >= 0x80 {
					d.logf("订阅 %s 被拒 0x%02x（等重连后再订）", f, code)
					d.resetDataSubscriptions()
				}
			}
		}
	}
}

func (d *Device) resetDataSubscriptions() {
	d.mu.Lock()
	d.dataSubscribed = false
	d.mu.Unlock()
}

func (d *Device) onMessage(ctx context.Context, c paho.Client, kick chan<- struct{}, topic string, payload []byte) {
	parts := strings.Split(topic, "/")
	if len(parts) != 3 {
		return
	}
	switch parts[2] {
	case "pair":
		var p struct {
			Paired bool `json:"paired"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return
		}
		if p.Paired {
			d.logf("配对成功")
			d.mu.Lock()
			d.st.Paired = true
			d.st.Code = ""
			d.mu.Unlock()
			_ = d.save()
			d.setWaiting()
			// 不在 paho 的消息回调里同步等 SUBACK：服务端可能同时踢连接，
			// 阻塞会拖住连接丢失的处理。
			go d.subscribeData(c, kick)
			go d.fetchAsync(ctx)
			return
		}
		d.logf("收到解绑通知，准备重新注册")
		d.mu.Lock()
		d.st.Paired = false
		d.st.Token = ""
		d.st.Code = ""
		d.bundle = nil
		d.etag = ""
		d.needRegister = true
		d.dataSubscribed = false
		d.mu.Unlock()
		d.setWaiting()
		d.signalFrame()
	case "frame":
		var p struct {
			Ver uint64 `json:"ver"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return
		}
		d.logf("帧版本通知：%d", p.Ver)
		go d.fetchAsync(ctx)
	case "usage":
		d.logf("收到用量：%s", string(payload))
	}
}

func (d *Device) fetchAsync(ctx context.Context) {
	if err := d.FetchFrame(ctx); err != nil && !errors.Is(err, errNeedRereg) {
		d.logf("取帧失败：%v", err)
	}
}

// pushRetryDelays 是 DLNA 推送失败后的退避：5s、15s、60s。
var pushRetryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 60 * time.Second}

func (d *Device) pushToTV(ctx context.Context) {
	if d.stream == nil || d.cfg.TV == "" {
		return
	}
	u, err := pushWithRetry(ctx, d.cfg.TV, d.stream.Addr(), localIPToward,
		pushDLNA, pushRetryDelays, d.logf)
	if err != nil {
		if ctx.Err() == nil {
			d.logf("DLNA 推送失败：%v（已重试 %d 次）", err, len(pushRetryDelays))
		}
		return
	}
	d.logf("已推送到电视 %s：%s", d.cfg.TV, u)
}

// pushWithRetry 初次失败后按 delays 退避重试，每次重试都重新解析局域网地址，
// 网络恢复/变化后不需要重启模拟器。成功时返回推送用的 URL。
func pushWithRetry(
	ctx context.Context,
	tv, listenAddr string,
	ipFor func(string) (string, error),
	push func(context.Context, string, string) error,
	delays []time.Duration,
	logf func(string, ...any),
) (string, error) {
	var last error
	for attempt := 0; ; attempt++ {
		u, err := buildStreamURL(listenAddr, tv, ipFor)
		if err == nil {
			if err = push(ctx, tv, u); err == nil {
				return u, nil
			}
		}
		last = err
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if attempt >= len(delays) {
			return "", last
		}
		logf("DLNA 推送失败：%v；%s 后重试", err, delays[attempt])
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delays[attempt]):
		}
	}
}
