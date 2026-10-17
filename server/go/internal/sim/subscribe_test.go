package sim

import (
	"context"
	"testing"
	"time"
)

func TestSubackVerdict(t *testing.T) {
	cases := []struct {
		name   string
		codes  map[string]byte
		denied bool
		retry  bool
	}{
		{"granted qos1", map[string]byte{"a": 1}, false, false},
		{"unspecified 0x80", map[string]byte{"a": 0x80}, true, false},
		{"not authorized 0x87", map[string]byte{"a": 0x87}, true, false},
		{"filter invalid 0x8f", map[string]byte{"a": 0x8f}, true, false},
		{"packet id in use 0x91", map[string]byte{"a": 0x91}, false, true},
		{"one granted one retry", map[string]byte{"a": 1, "b": 0x91}, false, true},
		{"one granted one denied", map[string]byte{"a": 1, "b": 0x80}, true, false},
	}
	for _, c := range cases {
		denied, retry := subackVerdict(c.codes)
		if denied != c.denied || retry != c.retry {
			t.Errorf("%s: denied=%v retry=%v，期望 %v/%v",
				c.name, denied, retry, c.denied, c.retry)
		}
	}
}

// 0x91 是 broker 的报文号冲突，退避后应重试成功，不能退回去等配对。
func TestSubscribeWithRetryPacketIDInUse(t *testing.T) {
	calls := 0
	var filterCounts []int
	do := func(f map[string]byte) (map[string]byte, error) {
		calls++
		filterCounts = append(filterCounts, len(f))
		if calls < 3 {
			return map[string]byte{"usage": 0x91, "frame": 0x91}, nil
		}
		return map[string]byte{"usage": 1, "frame": 1}, nil
	}
	filters := map[string]byte{"usage": 1, "frame": 1}
	res := subscribeWithRetry(context.Background(), filters, do,
		[]time.Duration{time.Millisecond, time.Millisecond}, func(string, ...any) {})
	if res != subOK {
		t.Fatalf("res = %v，期望 subOK", res)
	}
	if calls != 3 {
		t.Errorf("订阅尝试 = %d 次，期望 3", calls)
	}
	for i, n := range filterCounts {
		if n != 2 {
			t.Errorf("第 %d 次只订了 %d 个主题；usage/frame 应合并成一个 SUBSCRIBE", i+1, n)
		}
	}
}

func TestSubscribeWithRetryGivesUpOnPersistentTemporaryError(t *testing.T) {
	calls := 0
	do := func(map[string]byte) (map[string]byte, error) {
		calls++
		return map[string]byte{"usage": 0x91}, nil
	}
	res := subscribeWithRetry(context.Background(), map[string]byte{"usage": 1}, do,
		[]time.Duration{time.Millisecond, time.Millisecond}, func(string, ...any) {})
	if res != subFailed {
		t.Errorf("res = %v，期望 subFailed（重连，而不是回退配对）", res)
	}
	if calls != 3 {
		t.Errorf("订阅尝试 = %d 次，期望 3", calls)
	}
}

func TestSubscribeWithRetryDeniedNoRetry(t *testing.T) {
	for _, code := range []byte{0x80, 0x87, 0x8f} {
		calls := 0
		do := func(map[string]byte) (map[string]byte, error) {
			calls++
			return map[string]byte{"usage": code}, nil
		}
		res := subscribeWithRetry(context.Background(), map[string]byte{"usage": 1}, do,
			[]time.Duration{time.Hour, time.Hour}, func(string, ...any) {})
		if res != subDenied {
			t.Errorf("0x%02x: res = %v，期望 subDenied", code, res)
		}
		if calls != 1 {
			t.Errorf("0x%02x: 被拒不应重试，实际 %d 次", code, calls)
		}
	}
}
