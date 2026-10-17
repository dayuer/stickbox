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
	"slices"
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
	subMu   sync.Mutex

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

	// 新连接：先订 pair。usage/frame 属于配对后才订阅的主题；
	// 本地已经是配对状态时直接订，不等 paired=true——服务端重启后
	// retained 可能丢了，但服务端现在也会按密钥状态补发。
	d.mu.Lock()
	d.dataSubscribed = false
	d.mu.Unlock()
	pairTopic := "printer/" + st.Dev + "/pair"
	res := subscribeWithRetry(ctx, map[string]byte{pairTopic: 1},
		func(filters map[string]byte) (map[string]byte, error) {
			return subscribeFilters(c, filters)
		}, subRetryWaits, d.logf)
	if res == subDenied {
		return fmt.Errorf("sim: 订阅 %s 被拒", pairTopic)
	}
	if res != subOK {
		return fmt.Errorf("sim: 订阅 %s 失败", pairTopic)
	}

	d.mu.RLock()
	wasPaired := d.st.Paired
	d.mu.RUnlock()
	if wasPaired {
		switch d.subscribeData(ctx, c) {
		case subOK:
			go d.fetchAsync(ctx)
		case subDenied:
			// 本地以为配对了，实际还是 pending：退回等待配对。
			d.logf("直接订阅 usage/frame 被拒，实际仍是待配对，回退等待配对")
			d.mu.Lock()
			d.st.Paired = false
			d.st.Code = ""
			d.mu.Unlock()
			_ = d.save()
			d.setWaiting()
			go func() {
				if _, err := d.Register(ctx); err != nil {
					d.logf("重新获取配对码失败：%v", err)
				}
			}()
		case subFailed:
			select {
			case kick <- struct{}{}:
			default:
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
		// 数据订阅失败（可能是身份还是 pending，也可能只是临时报文号冲突）；
		// 主动重连，重连后会再试一次。
		return errors.New("sim: 需要重连后重试订阅")
	case <-time.After(24 * time.Hour):
		return errors.New("sim: MQTT 会话到期")
	}
}

type subResult int

const (
	subOK     subResult = iota
	subDenied           // 真被拒（0x80/0x87/0x8F）：这把密钥还是 pending 等
	subFailed           // 重试后仍失败或超时：重连再试，不要回退配对
)

// subRetryWaits 是临时 SUBACK 错误（例如 0x91 Packet Identifier in use）
// 的重试间隔：先等 broker 把占用的报文号放掉。
var subRetryWaits = []time.Duration{500 * time.Millisecond, time.Second}

// subackVerdict 区分「真被拒」和「临时错误」。
//
// 0x80 Unspecified / 0x87 Not authorized / 0x8F Topic filter invalid
// 才是权限或主题问题；0x91 Packet Identifier in use 之类的只是并发
// 报文号冲突，退避重试即可。
func subackVerdict(codes map[string]byte) (denied, retry bool) {
	for _, code := range codes {
		switch code {
		case 0x80, 0x87, 0x8f:
			denied = true
		default:
			if code >= 0x80 {
				retry = true
			}
		}
	}
	return denied, retry
}

// subscribeFilters 发一次 SUBSCRIBE（多个主题合并成一个包，避免报文号
// 被重复占用），返回 SUBACK 的每个主题结果。
func subscribeFilters(c paho.Client, filters map[string]byte) (map[string]byte, error) {
	tok := c.SubscribeMultiple(filters, nil)
	if !tok.WaitTimeout(5 * time.Second) {
		return nil, errors.New("sim: 订阅超时")
	}
	if err := tok.Error(); err != nil {
		return nil, err
	}
	if sub, ok := tok.(*paho.SubscribeToken); ok {
		return sub.Result(), nil
	}
	return map[string]byte{}, nil
}

// subscribeWithRetry 对临时 SUBACK 错误按 waits 退避重试。
func subscribeWithRetry(
	ctx context.Context,
	filters map[string]byte,
	do func(map[string]byte) (map[string]byte, error),
	waits []time.Duration,
	logf func(string, ...any),
) subResult {
	for attempt := 0; ; attempt++ {
		codes, err := do(filters)
		if err != nil {
			logf("订阅失败：%v", err)
			return subFailed
		}
		denied, retry := subackVerdict(codes)
		if denied {
			logf("订阅被拒：%v", codes)
			return subDenied
		}
		if !retry {
			return subOK
		}
		if attempt >= len(waits) {
			logf("订阅一直返回临时错误：%v", codes)
			return subFailed
		}
		logf("订阅暂时失败（%v），%s 后重试", codes, waits[attempt])
		select {
		case <-ctx.Done():
			return subFailed
		case <-time.After(waits[attempt]):
		}
	}
}

// subscribeData 订阅 usage/frame（合并成一个 SUBSCRIBE）。全部成功后才
// 置 dataSubscribed，之后同一连接上的重复调用直接返回成功。
func (d *Device) subscribeData(ctx context.Context, c paho.Client) subResult {
	d.subMu.Lock()
	defer d.subMu.Unlock()

	d.mu.RLock()
	done, dev := d.dataSubscribed, d.st.Dev
	d.mu.RUnlock()
	if done {
		return subOK
	}

	filters := map[string]byte{
		"printer/" + dev + "/usage": 1,
		"printer/" + dev + "/frame": 1,
	}
	res := subscribeWithRetry(ctx, filters,
		func(filters map[string]byte) (map[string]byte, error) {
			return subscribeFilters(c, filters)
		}, subRetryWaits, d.logf)
	if res != subOK {
		return res
	}
	d.mu.Lock()
	d.dataSubscribed = true
	d.mu.Unlock()
	return subOK
}

// keyID 是本地设备令牌的 key_id 部分。
func (d *Device) keyID() string {
	d.mu.RLock()
	tok := d.st.Token
	d.mu.RUnlock()
	id, _, _ := strings.Cut(tok, ".")
	return id
}

func (d *Device) onMessage(ctx context.Context, c paho.Client, kick chan<- struct{}, topic string, payload []byte) {
	parts := strings.Split(topic, "/")
	if len(parts) != 3 {
		return
	}
	switch parts[2] {
	case "pair":
		var p struct {
			Paired bool     `json:"paired"`
			KeyID  string   `json:"key_id"`
			KeyIDs []string `json:"key_ids"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return
		}
		// 只认发给「自己这把密钥」的通知：同一台设备可能换过密钥，
		// 旧的 retained / 别人的通知不能把状态带偏。
		if p.Paired {
			if p.KeyID != "" && p.KeyID != d.keyID() {
				d.logf("忽略发给其他密钥的配对通知（key_id %s）", p.KeyID)
				return
			}
			d.logf("配对成功")
			d.mu.Lock()
			d.st.Paired = true
			d.st.Code = ""
			d.mu.Unlock()
			_ = d.save()
			d.setWaiting()
			// 不在 paho 的消息回调里同步等 SUBACK：服务端可能同时踢连接，
			// 阻塞会拖住连接丢失的处理。
			go func() {
				switch d.subscribeData(ctx, c) {
				case subOK:
					d.fetchAsync(ctx)
				case subDenied, subFailed:
					// 连接还是 pending 身份（服务端马上会踢），或网络问题：
					// 重连后本地 Paired=true，会直接订阅。
					select {
					case kick <- struct{}{}:
					default:
					}
				}
			}()
			return
		}
		if len(p.KeyIDs) > 0 && !slices.Contains(p.KeyIDs, d.keyID()) {
			d.logf("忽略其他密钥的解绑通知")
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
	u, pushed, err := pushWithRetry(ctx, d.cfg.TV, d.stream.Addr(), localIPToward,
		pushDLNA, d.stream, pushRetryDelays, d.logf)
	if err != nil {
		if ctx.Err() == nil {
			d.logf("DLNA 推送失败：%v（已重试 %d 次）", err, len(pushRetryDelays))
		}
		return
	}
	if !pushed {
		d.logf("电视已在拉流，跳过重推：%s", u)
		return
	}
	d.logf("已推送到电视 %s：%s", d.cfg.TV, u)
}

// pushWithRetry 初次失败后按 delays 退避重试，每次重试都重新解析局域网地址，
// 网络恢复/变化后不需要重启模拟器。电视已经有活跃拉流连接时直接跳过，
// 避免把正在播放的流掐断。成功时返回推送用的 URL 和是否真的推过。
func pushWithRetry(
	ctx context.Context,
	tv, listenAddr string,
	ipFor func(string) (string, error),
	push func(context.Context, string, string, streamProbe) error,
	probe streamProbe,
	delays []time.Duration,
	logf func(string, ...any),
) (string, bool, error) {
	var last error
	for attempt := 0; ; attempt++ {
		u, err := buildStreamURL(listenAddr, tv, ipFor)
		if err != nil {
			last = err
		} else {
			if probe != nil && probe.Active() {
				return u, false, nil
			}
			if err := push(ctx, tv, u, probe); err == nil {
				return u, true, nil
			} else {
				last = err
			}
			// 推完才发现电视其实已经连上来拉流了：别再 Stop 重推。
			if probe != nil && probe.Active() {
				return u, false, nil
			}
		}
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		if attempt >= len(delays) {
			return "", false, last
		}
		logf("DLNA 推送失败：%v；%s 后重试", last, delays[attempt])
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(delays[attempt]):
		}
	}
}
