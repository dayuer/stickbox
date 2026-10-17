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

const minimal = `{"cert_dir":"/etc/le/live/x"}`

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
}

func TestLoadRejectsMissingCertDir(t *testing.T) {
	if _, err := Load(write(t, `{"root":"/srv/ap"}`)); err == nil {
		t.Error("缺 cert_dir 时应当报错")
	}
}

func TestDerivedPaths(t *testing.T) {
	c, err := Load(write(t, `{"cert_dir":"/c","root":"/srv/ap"}`))
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
