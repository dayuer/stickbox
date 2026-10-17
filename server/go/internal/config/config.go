// Package config 读取 config.json。
//
// T7 起没有手机号、短信服务商和 App：配置只剩服务本身需要的项。
package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Root     string `json:"root"`
	CertDir  string `json:"cert_dir"`
	HTTPAddr string `json:"http_addr"`
	MQTTAddr string `json:"mqtt_addr"`
	// DefaultTZ 是 agent 没上报时区时的兜底（IANA 名，如 Asia/Bangkok）。
	DefaultTZ   string `json:"default_tz"`
	MetricsAddr string `json:"metrics_addr"`
	// AdminAddr 是设备诊断的只读 HTTP 接口，只允许回环地址。
	AdminAddr string `json:"admin_addr"`

	// TokenPepper 是令牌哈希的服务端密钥（HMAC-SHA256）。必填：
	// 缺了就不启动，避免退回无 pepper 的哈希。
	// 生成：openssl rand -base64 32；只放 config.json（0600），不进仓库。
	TokenPepper string `json:"token_pepper"`

	// MQTT 每连接缓冲上限。设备只有 3 个订阅、流量很低；
	// mochi 默认 8192/8192/1024 每连接约 1.4MB。
	MQTTMaxWritesPending int32  `json:"mqtt_max_writes_pending"`
	MQTTMaxInflight      uint16 `json:"mqtt_max_inflight"`
	MQTTReceiveMaximum   uint16 `json:"mqtt_receive_maximum"`
	// MQTTAdmitPerSec 是全局新连接准入（每秒），超过直接拒绝。
	MQTTAdmitPerSec int `json:"mqtt_admit_per_sec"`

	// 帧缓存：磁盘总量与常驻内存上限，都按最近取用淘汰。
	FrameCacheMaxBytes  int64 `json:"frame_cache_max_bytes"`
	FrameMemoryMaxBytes int64 `json:"frame_memory_max_bytes"`
	// BackfillPerSec 是启动后补帧速率（用户/秒），避免重连风暴。
	BackfillPerSec int `json:"backfill_users_per_sec"`

	Firmware Firmware `json:"firmware"`
}

// Firmware 是 OTA 通道要下发的版本。Version 或 URL 为空 = 不下发任何升级。
type Firmware struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`

	// 分阶段放量百分比（0–100）。字段缺省按 100 处理；显式写 0 则谁也不推。
	// 用指针是为了把「没写这个字段」和「写了 0」分开——两者后果相反。
	RolloutPercent *int `json:"rollout_percent"`
}

func (f Firmware) RolloutPercentOrDefault() int {
	if f.RolloutPercent == nil {
		return 100
	}
	return *f.RolloutPercent
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.CertDir == "" {
		return nil, errors.New("config: cert_dir 必填")
	}
	if c.Root == "" {
		c.Root = "/opt/stickbox"
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":9443"
	}
	if c.MQTTAddr == "" {
		c.MQTTAddr = ":8883"
	}
	if c.DefaultTZ == "" {
		c.DefaultTZ = "UTC"
	}
	if c.MetricsAddr == "" {
		// 只监听回环：指标不经过 nginx，也不对公网开放。
		c.MetricsAddr = "127.0.0.1:9090"
	}
	if c.AdminAddr == "" {
		c.AdminAddr = "127.0.0.1:9091"
	}
	if err := checkLoopbackAddr("admin_addr", c.AdminAddr); err != nil {
		return nil, err
	}
	c.TokenPepper = strings.TrimSpace(c.TokenPepper)
	if err := checkPepper(c.TokenPepper); err != nil {
		return nil, err
	}
	if c.MQTTMaxWritesPending <= 0 {
		c.MQTTMaxWritesPending = 64
	}
	if c.MQTTMaxInflight == 0 {
		c.MQTTMaxInflight = 32
	}
	if c.MQTTReceiveMaximum == 0 {
		c.MQTTReceiveMaximum = 32
	}
	if c.MQTTAdmitPerSec <= 0 {
		c.MQTTAdmitPerSec = 50
	}
	if c.FrameCacheMaxBytes <= 0 {
		c.FrameCacheMaxBytes = 256 << 20
	}
	if c.FrameMemoryMaxBytes <= 0 {
		c.FrameMemoryMaxBytes = 64 << 20
	}
	if c.BackfillPerSec <= 0 {
		c.BackfillPerSec = 2
	}
	return &c, nil
}

// checkLoopbackAddr 只允许诊断接口绑定回环地址。设备诊断包含用户/密钥
// 元数据，绝对不能因为配置写错而暴露到公网。
func checkLoopbackAddr(name, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("config: %s 不合法：%w", name, err)
	}
	if host == "" {
		return fmt.Errorf("config: %s 必须显式绑定回环地址（如 127.0.0.1:9091）", name)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("config: %s 只允许回环地址，当前 %q", name, addr)
	}
	return nil
}

// checkPepper 拒绝空值、示例占位符和熵不足的 pepper。
// openssl rand -base64 32 解出来是 32 字节；短于 16 字节没有意义。
func checkPepper(p string) error {
	if p == "" {
		return errors.New("config: token_pepper 必填（openssl rand -base64 32）")
	}
	lower := strings.ToLower(p)
	for _, bad := range []string{"openssl", "请用", "changeme", "placeholder", "example", "your-"} {
		if strings.Contains(lower, bad) {
			return errors.New("config: token_pepper 仍是示例占位符，请生成随机值")
		}
	}
	key := []byte(p)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if dec, err := enc.DecodeString(p); err == nil {
			key = dec
			break
		}
	}
	if len(key) < 16 {
		return errors.New("config: token_pepper 解码后至少需要 16 字节")
	}
	return nil
}

// DBPath 沿用 jobs.db 这个文件名：已有部署的数据都在这只库里，
// 改名等于让升级丢数据。
func (c *Config) DBPath() string   { return filepath.Join(c.Root, "jobs.db") }
func (c *Config) CertPath() string { return filepath.Join(c.CertDir, "fullchain.pem") }
func (c *Config) KeyPath() string  { return filepath.Join(c.CertDir, "privkey.pem") }
