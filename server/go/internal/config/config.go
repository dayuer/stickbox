// Package config 读取 config.json。
//
// T7 起没有手机号、短信服务商和 App：配置只剩服务本身需要的项。
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type Config struct {
	Root     string `json:"root"`
	CertDir  string `json:"cert_dir"`
	HTTPAddr string `json:"http_addr"`
	MQTTAddr string `json:"mqtt_addr"`
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
	return &c, nil
}

// DBPath 沿用 jobs.db 这个文件名：已有部署的数据都在这只库里，
// 改名等于让升级丢数据。
func (c *Config) DBPath() string   { return filepath.Join(c.Root, "jobs.db") }
func (c *Config) CertPath() string { return filepath.Join(c.CertDir, "fullchain.pem") }
func (c *Config) KeyPath() string  { return filepath.Join(c.CertDir, "privkey.pem") }
