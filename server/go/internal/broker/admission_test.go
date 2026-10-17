package broker

import (
	"errors"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

func TestAdmissionTokenBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	a := newAdmission(2)
	a.now = func() time.Time { return now }
	a.global.last = now
	const ip = "192.0.2.1:5555"

	if !a.allow(ip) || !a.allow(ip) {
		t.Fatal("前两个连接应放行")
	}
	if a.allow(ip) {
		t.Fatal("burst 用完后的第三个连接应被拒")
	}
	now = now.Add(500 * time.Millisecond) // 补 1 个令牌
	if !a.allow(ip) {
		t.Fatal("等待半个周期后应放行 1 个")
	}
	if a.allow(ip) {
		t.Fatal("只补了 1 个令牌，不应放行两个")
	}
	now = now.Add(10 * time.Second) // 上限不超过 burst
	if !a.allow(ip) || !a.allow(ip) {
		t.Fatal("长时间空闲后应恢复 burst")
	}
	if a.allow(ip) {
		t.Fatal("burst 只有 2，不应放行第三个")
	}
}

// 单个来源不能吞掉全局额度、把其他设备挡在门外。
func TestAdmissionPerIPIsolation(t *testing.T) {
	now := time.Unix(1000, 0)
	a := newAdmission(1000) // 全局额度充足，只看每 IP
	a.now = func() time.Time { return now }
	a.global.last = now

	for i := 0; i < perIPBurst; i++ {
		if !a.allow("192.0.2.9:1000") {
			t.Fatalf("同一 IP 的第 %d 个连接被拒", i+1)
		}
	}
	if a.allow("192.0.2.9:1001") {
		t.Fatal("同一 IP 超过 burst 后仍被放行")
	}
	if !a.allow("192.0.2.10:1000") {
		t.Fatal("另一个来源 IP 被误伤")
	}
}

// 准入限流必须回 0x03（server unavailable），不能回 0x05：固件会把 0x05
// 当成密钥被吊销而清 NVS、重新注册。
func TestAdmissionRejectsWithServerUnavailable(t *testing.T) {
	now := time.Unix(1000, 0)
	a := newAdmission(1)
	a.now = func() time.Time { return now }
	a.global.last = now
	if !a.allow("192.0.2.1:1111") {
		t.Fatal("第一个连接应放行")
	}
	h := &Hook{admit: a}
	cl := &mqtt.Client{Net: mqtt.ClientConnection{Remote: "192.0.2.1:2222"}}
	err := h.OnConnect(cl, packets.Packet{})
	if !errors.Is(err, packets.ErrServerUnavailable) {
		t.Fatalf("OnConnect = %v，期望 packets.ErrServerUnavailable（0x03）", err)
	}
}

func TestBrokerAppliesCapabilities(t *testing.T) {
	v := auth.NewVerifier(nil, "test-pepper-0123456789abcdef")
	b, err := New("127.0.0.1:0", nil, v, nil, Options{
		MaximumClientWritesPending: 64,
		MaximumInflight:            32,
		ReceiveMaximum:             32,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	caps := b.srv.Options.Capabilities
	if caps.MaximumClientWritesPending != 64 || caps.MaximumInflight != 32 || caps.ReceiveMaximum != 32 {
		t.Errorf("Capabilities = %d/%d/%d，期望 64/32/32",
			caps.MaximumClientWritesPending, caps.MaximumInflight, caps.ReceiveMaximum)
	}
}
