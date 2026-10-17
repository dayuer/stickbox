package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `{"cert_dir":"/etc/le/live/x","token_pepper":"0123456789abcdefghijklmn"}`

func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Root != "/opt/stickbox" {
		t.Errorf("Root = %q，期望默认 /opt/stickbox", c.Root)
	}
	if c.HTTPAddr != ":9443" {
		t.Errorf("HTTPAddr = %q，期望默认 :9443", c.HTTPAddr)
	}
	if c.MQTTAddr != ":8883" {
		t.Errorf("MQTTAddr = %q，期望默认 :8883", c.MQTTAddr)
	}
	if c.MQTTMaxWritesPending != 64 || c.MQTTMaxInflight != 32 || c.MQTTReceiveMaximum != 32 {
		t.Errorf("MQTT 缓冲默认 = %d/%d/%d，期望 64/32/32",
			c.MQTTMaxWritesPending, c.MQTTMaxInflight, c.MQTTReceiveMaximum)
	}
	if c.BackfillPerSec != 2 || c.FrameCacheMaxBytes == 0 {
		t.Errorf("帧缓存/补帧默认没生效：%+v", c)
	}
}

func TestLoadRejectsMissingPepper(t *testing.T) {
	if _, err := Load(write(t, `{"cert_dir":"/c"}`)); err == nil {
		t.Error("缺 token_pepper 时应当拒绝启动")
	}
	if _, err := Load(write(t, `{"cert_dir":"/c","token_pepper":"short"}`)); err == nil {
		t.Error("token_pepper 太短时应当拒绝启动")
	}
	if _, err := Load(write(t,
		`{"cert_dir":"/c","token_pepper":"请用 openssl rand -base64 32 生成，至少 16 字符"}`)); err == nil {
		t.Error("示例占位符应当被拒绝")
	}
	if _, err := Load(write(t,
		`{"cert_dir":"/c","token_pepper":"  short-but-padded  "}`)); err == nil {
		t.Error("TrimSpace 后不足 16 字节应当被拒绝")
	}
}

func TestLoadRejectsMissingCertDir(t *testing.T) {
	if _, err := Load(write(t, `{"root":"/srv/ap","token_pepper":"0123456789abcdefghijklmn"}`)); err == nil {
		t.Error("缺 cert_dir 时应当报错")
	}
}

func TestDerivedPaths(t *testing.T) {
	c, err := Load(write(t, `{"cert_dir":"/c","root":"/srv/ap","token_pepper":"0123456789abcdefghijklmn"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.DBPath() != "/srv/ap/jobs.db" {
		t.Errorf("DBPath = %q", c.DBPath())
	}
	if c.CertPath() != "/c/fullchain.pem" {
		t.Errorf("CertPath = %q", c.CertPath())
	}
	if c.KeyPath() != "/c/privkey.pem" {
		t.Errorf("KeyPath = %q", c.KeyPath())
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	if _, err := Load("/nope/config.json"); err == nil {
		t.Error("配置文件不存在时应当报错")
	}
}
