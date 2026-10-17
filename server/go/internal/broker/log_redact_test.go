package broker

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

// 认证失败日志只应出现 username（dev）和来源地址，不能出现密码令牌。
func TestAuthFailureLogDoesNotLeakToken(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	defer slog.SetDefault(old)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	h := &Hook{v: auth.NewVerifier(st)}
	token := "a3f91c04bd77.kJ8xQ2mN4pL7vR1sT6wY9zA0bC3dE5fH"
	pk := packets.Packet{Connect: packets.ConnectParams{
		Username: []byte("f412fa87c9e0"),
		Password: []byte(token),
	}}
	if h.OnConnectAuthenticate(&mqtt.Client{}, pk) {
		t.Fatal("无效令牌不应通过认证")
	}

	logs := buf.String()
	if !strings.Contains(logs, "MQTT 认证失败") {
		t.Fatalf("认证失败路径没有写日志，测试无效:\n%s", logs)
	}
	if strings.Contains(logs, token) {
		t.Errorf("日志泄露了令牌值:\n%s", logs)
	}
	if strings.Contains(logs, "Bearer") {
		t.Errorf("日志出现了 Authorization 头内容:\n%s", logs)
	}
}
