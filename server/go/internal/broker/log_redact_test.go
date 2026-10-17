package broker

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	h := &Hook{v: auth.NewVerifier(st, "test-pepper-0123456789abcdef"), ids: newIDTable()}
	token := "a3f91c04bd77.kJ8xQ2mN4pL7vR1sT6wY9zA0bC3dE5fH"
	pk := packets.Packet{Connect: packets.ConnectParams{
		Username: []byte("f412fa87c9e0"),
		Password: []byte(token),
	}}
	cl := &mqtt.Client{}
	if err := h.OnConnect(cl, pk); err != nil {
		t.Fatalf("无效令牌应交给 OnConnectAuthenticate 回 0x05，而不是 %v", err)
	}
	if h.OnConnectAuthenticate(cl, pk) {
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

// mochi 在 ACL 拒绝时会以 WARN 打出整个 packet（含 payload），每包几 KB。
// 处理器要把 pk 替换掉，但保留 error/client 这些排障信息。
func TestPacketRedactHandlerDropsPayload(t *testing.T) {
	var buf bytes.Buffer
	h := packetRedactHandler{slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})}
	r := slog.NewRecord(time.Now(), slog.LevelWarn, "error processing packet", 0)
	r.AddAttrs(
		slog.String("error", "not authorized"),
		slog.String("client", "f412fa87c9e0"),
		slog.String("pk", strings.Repeat("SECRET-PAYLOAD-", 500)),
	)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "SECRET-PAYLOAD") {
		t.Errorf("日志仍然包含 payload：\n%s", out)
	}
	if !strings.Contains(out, "pk=<redacted>") {
		t.Errorf("pk 没有被替换：\n%s", out)
	}
	if !strings.Contains(out, "not authorized") || !strings.Contains(out, "f412fa87c9e0") {
		t.Errorf("排障字段被误删：\n%s", out)
	}
}
