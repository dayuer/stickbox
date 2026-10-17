package registry

import (
	"sync"
	"testing"
	"time"
)

func newReg(t *testing.T, idle time.Duration, now func() time.Time) (*Registry, chan time.Time) {
	t.Helper()
	tick := make(chan time.Time)
	r := New(Options{IdleTimeout: idle, Ticker: tick, Now: now})
	t.Cleanup(r.Shutdown)
	return r, tick
}

func TestSeenMarksDeviceOnline(t *testing.T) {
	now := time.Unix(1000, 0)
	r, _ := newReg(t, time.Hour, func() time.Time { return now })

	r.Seen("d1")
	if !r.IsOnline("d1") {
		t.Fatal("心跳后设备不在线")
	}
	if got := r.Online(); len(got) != 1 || got[0] != "d1" {
		t.Fatalf("在线列表 = %v，期望 [d1]", got)
	}
}

func TestIdleDeviceExpires(t *testing.T) {
	now := time.Unix(1000, 0)
	r, tick := newReg(t, 120*time.Second, func() time.Time { return now })
	r.Seen("d1")

	now = now.Add(121 * time.Second)
	tick <- now
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !r.IsOnline("d1") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("超过 IdleTimeout 后设备仍在线")
}

func TestHeartbeatRefreshesDevice(t *testing.T) {
	now := time.Unix(1000, 0)
	r, tick := newReg(t, 120*time.Second, func() time.Time { return now })
	r.Seen("d1")

	now = now.Add(100 * time.Second)
	r.Seen("d1") // 第二拍
	now = now.Add(100 * time.Second)
	tick <- now // 距第一拍 200 秒，距第二拍 100 秒
	time.Sleep(50 * time.Millisecond)
	if !r.IsOnline("d1") {
		t.Fatal("设备持续心跳却被判离线")
	}
}

func TestConcurrentSeen(t *testing.T) {
	r, _ := newReg(t, time.Hour, time.Now)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Seen("d1")
		}()
	}
	wg.Wait()
	if len(r.Online()) != 1 {
		t.Errorf("并发心跳记出了 %d 台设备", len(r.Online()))
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	r, _ := newReg(t, time.Hour, time.Now)
	r.Seen("d1")
	r.Shutdown()
	r.Shutdown() // 第二次不能 panic
	if len(r.Online()) != 1 {
		t.Error("关闭后在线表不该被清空——关闭不改变已经观测到的事实")
	}
}
