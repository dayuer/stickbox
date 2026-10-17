package source

import (
	"context"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/usage"
)

func TestClaudeSnapshot(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	received := now.Add(-3 * time.Minute)
	c := Claude{
		Report: usage.Report{
			FiveHour: &usage.Window{UsedPercentage: 37.5, ResetsAt: now.Add(2 * time.Hour).Unix()},
			SevenDay: &usage.Window{UsedPercentage: 19.5, ResetsAt: now.Add(4 * 24 * time.Hour).Unix()},
		},
		ReceivedAt: received,
		DeviceIP:   "192.168.3.14",
	}
	s, err := c.Snapshot(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Now.Equal(now) || !s.LastUpdate.Equal(received) || s.DeviceIP != "192.168.3.14" {
		t.Fatalf("快照元数据不对: %+v", s)
	}
	if len(s.Windows) != 2 {
		t.Fatalf("窗口数 = %d，要 2", len(s.Windows))
	}
	w5 := s.Windows[0]
	if w5.Label != "5 小时" || w5.Length != 5*time.Hour {
		t.Errorf("5 小时窗口 = %+v", w5)
	}
	if w5.UsedPercentage != .375 {
		t.Errorf("5 小时 used = %v，要 0.375（百分比已换算）", w5.UsedPercentage)
	}
	w7 := s.Windows[1]
	if w7.Label != "7 天" || w7.Length != 7*24*time.Hour {
		t.Errorf("7 天窗口 = %+v", w7)
	}
	if w7.UsedPercentage != .195 {
		t.Errorf("7 天 used = %v，要 0.195", w7.UsedPercentage)
	}
}

func TestClaudeNoData(t *testing.T) {
	s, err := (Claude{DeviceIP: "10.0.0.2"}).Snapshot(context.Background(), time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Windows) != 0 {
		t.Fatalf("没有 report 时不应有窗口: %+v", s.Windows)
	}
	if !s.LastUpdate.IsZero() {
		t.Errorf("没有上报时 LastUpdate 应为零值")
	}
}
