package broker

import (
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

func TestAdmissionTokenBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	a := newAdmission(2)
	a.now = func() time.Time { return now }
	a.last = now

	if !a.allow() || !a.allow() {
		t.Fatal("前两个连接应放行")
	}
	if a.allow() {
		t.Fatal("burst 用完后的第三个连接应被拒")
	}
	now = now.Add(500 * time.Millisecond) // 补 1 个令牌
	if !a.allow() {
		t.Fatal("等待半个周期后应放行 1 个")
	}
	if a.allow() {
		t.Fatal("只补了 1 个令牌，不应放行两个")
	}
	now = now.Add(10 * time.Second) // 上限不超过 burst
	if !a.allow() || !a.allow() {
		t.Fatal("长时间空闲后应恢复 burst")
	}
	if a.allow() {
		t.Fatal("burst 只有 2，不应放行第三个")
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
