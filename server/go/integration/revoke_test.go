package integration

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
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

// rawConnack 发一个最小 MQTT 3.1.1 CONNECT，返回 CONNACK 的返回码。
// 用它才能精确断言 0x05，而不是只看 paho 的错误字符串。
func rawConnack(t *testing.T, addr, clientID, username, password string) byte {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("TLS 连接 %s：%v", addr, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	var payload bytes.Buffer
	writeStr := func(s string) {
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(s)))
		payload.Write(l[:])
		payload.WriteString(s)
	}
	writeStr(clientID)
	writeStr(username)
	writeStr(password)

	var vh bytes.Buffer
	vh.WriteString("\x00\x04MQTT\x04")
	vh.WriteByte(0xC2) // username + password + clean session
	vh.Write([]byte{0x00, 0x3c})
	vh.Write(payload.Bytes())

	var pkt bytes.Buffer
	pkt.WriteByte(0x10)
	writeRemaining(&pkt, vh.Len())
	pkt.Write(vh.Bytes())
	if _, err := conn.Write(pkt.Bytes()); err != nil {
		t.Fatalf("发 CONNECT：%v", err)
	}

	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		t.Fatalf("读 CONNACK：%v", err)
	}
	if hdr[0] != 0x20 || hdr[1] != 0x02 {
		t.Fatalf("不是 CONNACK：% x", hdr)
	}
	return hdr[3]
}

func writeRemaining(w io.Writer, n int) {
	for {
		b := byte(n % 128)
		n /= 128
		if n > 0 {
			b |= 0x80
		}
		_, _ = w.Write([]byte{b})
		if n == 0 {
			return
		}
	}
}

// 正式服务的复现：CLI 在另一个进程吊销密钥后，服务端缓存不能让旧密钥
// 继续 CONNECT。下一次 CONNECT 必须回 0x05（not authorized）。
func TestEndToEndRevokedKeyConnackNotAuthorized(t *testing.T) {
	const dev = "f412fa87c9e0"
	e := startServer(t)
	key, code := e.register(dev)
	e.pair(code, "")

	// 先成功连一次，确保服务端缓存（旧实现）是热的。
	d := e.connectDevice(dev, key)
	d.disconnect()
	time.Sleep(100 * time.Millisecond)

	if rc := rawConnack(t, e.mqttAddr, dev, dev, key); rc != 0 {
		t.Fatalf("吊销前 CONNACK = 0x%02x，期望 0", rc)
	}

	// 模拟 CLI：直接改库，不调用 Verifier.Invalidate。
	if _, err := e.store.DisableKeysOfDevice(dev, string(auth.RoleDevice)); err != nil {
		t.Fatal(err)
	}
	if rc := rawConnack(t, e.mqttAddr, dev, dev, key); rc != 5 {
		t.Fatalf("吊销后 CONNACK = 0x%02x，期望 0x05", rc)
	}
}

// 解绑后 pair=false 是 noAck 的 retained 通知：不带 cid、不重投；新注册
// 的 pending 密钥不会再收到发给旧密钥的指令，也不会因发布 ack 被 ACL 拒。
func TestEndToEndUnpairPairIsNoAck(t *testing.T) {
	const dev = "f412fa87c9e0"
	e := startServer(t)
	devKey, code := e.register(dev)
	agentTok, pairedDev, _ := e.pair(code, "")
	d := e.connectDevice(dev, devKey)
	if err := d.subscribePair(); err != nil {
		t.Fatal(err)
	}
	pairPayload(t, d.waitPair(5*time.Second))

	if n := e.cmd.Pending(dev); n != 0 {
		t.Fatalf("配对后不应有被跟踪的 pair：pending=%d", n)
	}
	e.unpair(agentTok, pairedDev)
	payload := d.waitPair(5 * time.Second)
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("pair 不是 JSON：%s", payload)
	}
	if m["paired"] != false {
		t.Fatalf("解绑通知 = %s", payload)
	}
	if _, ok := m["cid"]; ok {
		t.Errorf("解绑通知不应带 cid：%s", payload)
	}
	if n := e.cmd.Pending(dev); n != 0 {
		t.Errorf("解绑后不应有被跟踪的 pair：pending=%d", n)
	}
}

// unbind → 旧 key 名下的未确认指令作废；新密钥注册后不会收到旧 cid。
func TestEndToEndUnbindNewKeyNoOldCommands(t *testing.T) {
	const dev = "f412fa87c9e0"
	e := startServer(t)
	devKey, code := e.register(dev)
	agentTok, pairedDev, _ := e.pair(code, "")
	d := e.connectDevice(dev, devKey)
	if err := d.subscribeUsage(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cmd.Send(dev, "printer/"+dev+"/usage", []byte(`{"now":1}`), false); err != nil {
		t.Fatal(err)
	}
	cidOf(t, d.waitUsage(5*time.Second))
	if e.cmd.Pending(dev) != 1 {
		t.Fatalf("下行后待确认 = %d，期望 1", e.cmd.Pending(dev))
	}

	e.unpair(agentTok, pairedDev)
	if e.cmd.Pending(dev) != 0 {
		t.Fatalf("解绑后旧 cid 仍挂着：%d", e.cmd.Pending(dev))
	}
	if o, ok := e.cmd.LastOutcome(dev); !ok || o.Err != "unpaired" {
		t.Fatalf("结局 = %+v ok=%v，期望 unpaired", o, ok)
	}

	pendingKey, _ := e.register(dev)
	p := e.connectDevice(dev, pendingKey)
	if err := p.subscribePair(); err != nil {
		t.Fatal(err)
	}
	payload := p.waitPair(5 * time.Second)
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("pair 不是 JSON：%s", payload)
	}
	if m["paired"] != false {
		t.Fatalf("新密钥收到 = %s，期望 paired=false", payload)
	}
	if _, ok := m["cid"]; ok {
		t.Errorf("新密钥收到了带 cid 的旧指令：%s", payload)
	}
	if e.cmd.Pending(dev) != 0 {
		t.Errorf("新密钥连接后仍有旧指令待确认：%d", e.cmd.Pending(dev))
	}
}
