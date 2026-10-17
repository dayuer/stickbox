package source

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/sales"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

type fakeUsageStore struct {
	rep      usage.Report
	received time.Time
	ok       bool
	err      error
}

func (f fakeUsageStore) Latest(string) (usage.Report, time.Time, bool, error) {
	return f.rep, f.received, f.ok, f.err
}

var multiNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func usageAt(received time.Time) fakeUsageStore {
	return fakeUsageStore{
		ok:       true,
		received: received,
		rep: usage.Report{
			TZ:       "UTC",
			FiveHour: &usage.Window{UsedPercentage: 40, ResetsAt: multiNow.Add(time.Hour).Unix()},
		},
	}
}

func salesAt(received time.Time) fakeSalesStore {
	return fakeSalesStore{
		ok:       true,
		received: received,
		rep: sales.Report{
			Date: "2026-10-17", TZ: "UTC",
			GMV:    &sales.Metric{Value: 146_000, Goal: 200_000},
			Orders: &sales.Metric{Value: 420, Goal: 400},
		},
	}
}

// 哪一路上屏跟着最近一次上报走，不需要配置项。
func TestMostRecentPicksFreshest(t *testing.T) {
	cases := []struct {
		name               string
		usageAgo, salesAgo time.Duration
		wantDir            Direction
		wantFirstLabel     string
	}{
		{"经营数据更新", 30 * time.Minute, 2 * time.Minute, Target, "今日成交额"},
		{"额度更新", 2 * time.Minute, 30 * time.Minute, Budget, "5 小时"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMostRecent(
				NewClaude(usageAt(multiNow.Add(-tc.usageAgo)), "UTC"),
				NewSales(salesAt(multiNow.Add(-tc.salesAgo)), "UTC"),
			)
			s, err := m.Snapshot(context.Background(), "u1", multiNow)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Windows) == 0 {
				t.Fatal("没有窗口")
			}
			if s.Windows[0].Label != tc.wantFirstLabel {
				t.Errorf("首个窗口 = %q，要 %q", s.Windows[0].Label, tc.wantFirstLabel)
			}
			if s.Windows[0].Dir != tc.wantDir {
				t.Errorf("方向 = %v，要 %v", s.Windows[0].Dir, tc.wantDir)
			}
		})
	}
}

// 只报了一路时就显示那一路，另一路没数据不该把它盖掉。
func TestMostRecentIgnoresEmptySource(t *testing.T) {
	m := NewMostRecent(
		NewClaude(fakeUsageStore{}, "UTC"), // 从没上报过
		NewSales(salesAt(multiNow.Add(-time.Minute)), "UTC"),
	)
	s, err := m.Snapshot(context.Background(), "u1", multiNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Windows) != 2 || s.Windows[0].Label != "今日成交额" {
		t.Fatalf("应当显示经营看板: %+v", s.Windows)
	}
}

// 一路都没数据时要出占位快照，并且带着第一路的方向，让文案口径对得上。
func TestMostRecentNoDataAtAll(t *testing.T) {
	m := NewMostRecent(
		NewClaude(fakeUsageStore{}, "UTC"),
		NewSales(fakeSalesStore{}, "UTC"),
	)
	s, err := m.Snapshot(context.Background(), "u1", multiNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Windows) != 0 || s.HasData() {
		t.Fatalf("不该有窗口: %+v", s)
	}
	if s.Dir != Budget {
		t.Errorf("占位方向 = %v，要第一路的 Budget", s.Dir)
	}
}

// 一路挂了不该把另一路拖下水：显示还能用的那块板比显示空白好。
func TestMostRecentSurvivesOneFailure(t *testing.T) {
	boom := errors.New("数据库炸了")
	m := NewMostRecent(
		NewClaude(fakeUsageStore{err: boom}, "UTC"),
		NewSales(salesAt(multiNow.Add(-time.Minute)), "UTC"),
	)
	s, err := m.Snapshot(context.Background(), "u1", multiNow)
	if err != nil {
		t.Fatalf("一路失败不该整体报错: %v", err)
	}
	if len(s.Windows) != 2 || s.Windows[0].Dir != Target {
		t.Fatalf("应当回落到经营看板: %+v", s.Windows)
	}
}

// 全挂了才往上报错——帧服务会保留上一张好帧。
func TestMostRecentAllFail(t *testing.T) {
	boom := errors.New("数据库炸了")
	m := NewMostRecent(
		NewClaude(fakeUsageStore{err: boom}, "UTC"),
		NewSales(fakeSalesStoreErr{boom}, "UTC"),
	)
	if _, err := m.Snapshot(context.Background(), "u1", multiNow); !errors.Is(err, boom) {
		t.Fatalf("err = %v，要 %v", err, boom)
	}
}

type fakeSalesStoreErr struct{ err error }

func (f fakeSalesStoreErr) Latest(string) (sales.Report, time.Time, bool, error) {
	return sales.Report{}, time.Time{}, false, f.err
}

func TestMostRecentID(t *testing.T) {
	m := NewMostRecent(NewClaude(fakeUsageStore{}, "UTC"), NewSales(fakeSalesStore{}, "UTC"))
	if got := m.ID(); got != "claude-usage+chaodai-sales" {
		t.Errorf("ID = %q", got)
	}
}
