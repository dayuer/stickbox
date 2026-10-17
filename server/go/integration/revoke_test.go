package integration

import (
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

func waitDisconnected(t *testing.T, d *fakeDevice, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !d.c.IsConnected() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("旧连接没有被断开")
}

// 运维 CLI 的 device unbind/revoke 直接改库，不和 broker 同进程，没法踢线。
// 设备被吊销后重新注册、拿 pending 密钥用同一个 client ID 连回来时，
// 服务端必须发现那条 active 连接的密钥已经失效，允许 pending 接入并踢掉旧连接。
func TestEndToEndRevokedActiveKeyAllowsPendingReconnect(t *testing.T) {
	const dev = "f412fa87c9e0"
	e := startServer(t)
	activeKey, code := e.register(dev)
	e.pair(code, "")

	active := e.connectDevice(dev, activeKey)
	if err := active.subscribeUsage(); err != nil {
		t.Fatalf("active 订阅 usage：%v", err)
	}

	// 模拟 CLI：只吊销密钥，不经过 broker。
	if _, err := e.store.DisableKeysOfDevice(dev, string(auth.RoleDevice)); err != nil {
		t.Fatal(err)
	}

	pendingKey, _ := e.register(dev)
	p, err := e.tryConnectDevice(dev, pendingKey)
	if err != nil {
		t.Fatalf("吊销后新的 pending 密钥应能接入：%v", err)
	}
	if err := p.subscribePair(); err != nil {
		t.Fatalf("pending 订阅 pair：%v", err)
	}
	waitDisconnected(t, active, 3*time.Second)
}

// 即使设备不重连，服务端也应在下一次心跳时发现密钥已吊销并主动断开，
// 不能让一把已吊销的密钥继续收用量/帧。
func TestEndToEndRevokedKeyDisconnectedOnHeartbeat(t *testing.T) {
	const dev = "f412fa87c9e0"
	e := startServer(t)
	key, code := e.register(dev)
	e.pair(code, "")

	d := e.connectDevice(dev, key)
	d.heartbeat()

	if _, err := e.store.DisableKeysOfDevice(dev, string(auth.RoleDevice)); err != nil {
		t.Fatal(err)
	}
	// 只发不等：服务端处理心跳时会把连接踢掉，PUBACK 可能等不到。
	d.c.Publish("printer/"+dev+"/status", 1, false,
		[]byte(`{"dev":"`+dev+`","state":"online"}`))
	waitDisconnected(t, d, 3*time.Second)
}
