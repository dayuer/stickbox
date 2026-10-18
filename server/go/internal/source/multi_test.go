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
			SevenDay: &usage.Window{UsedPercentage: 20, ResetsAt: multiNow.Add(96 * time.Hour).Unix()},
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

func labels(s Snapshot) []string {
	out := make([]string, 0, len(s.Windows))
	for _, w := range s.Windows {
		out = append(out, w.Label)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 看板形态按有数据的源的数量切：一路独占两个环，两路各占一个环。
func TestCompositeSwitchesBySourceCount(t *testing.T) {
	ago := func(d time.Duration) time.Time { return multiNow.Add(-d) }
	cases := []struct {
		name  string
		usage fakeUsageStore
		sales fakeSalesStore
		want  []string
	}{
		{"只有额度：独占两环", usageAt(ago(time.Minute)), fakeSalesStore{},
			[]string{"5 小时", "7 天"}},
		{"只有经营数据：独占两环", fakeUsageStore{}, salesAt(ago(time.Minute)),
			[]string{"今日成交额", "今日订单"}},
		// 两路都有：各让一个环，每路只出 Windows[0]。
		// 排序按上报新鲜度，所以经营数据在前。
		{"两路：各占一环", usageAt(ago(30 * time.Minute)), salesAt(ago(time.Minute)),
			[]string{"今日成交额", "5 小时"}},
		{"两路：额度更新", usageAt(ago(time.Minute)), salesAt(ago(30 * time.Minute)),
			[]string{"5 小时", "今日成交额"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewComposite(
				NewClaude(tc.usage, NewFixedTZ("UTC")),
				NewSales(tc.sales, NewFixedTZ("UTC")),
			)
			s, err := c.Snapshot(context.Background(), "u1", multiNow)
			if err != nil {
				t.Fatal(err)
			}
			if got := labels(s); !eq(got, tc.want) {
				t.Errorf("窗口 = %v，要 %v", got, tc.want)
			}
		})
	}
}

// 两路混合时底栏只有一个「更新 X 前」，要取更旧的那个：
// 不能用新鲜的那路把旧的盖掉。
func TestCompositeMergedLastUpdateIsOldest(t *testing.T) {
	old := multiNow.Add(-3 * time.Hour)
	fresh := multiNow.Add(-time.Minute)
	c := NewComposite(
		NewClaude(usageAt(old), NewFixedTZ("UTC")),
		NewSales(salesAt(fresh), NewFixedTZ("UTC")),
	)
	s, err := c.Snapshot(context.Background(), "u1", multiNow)
	if err != nil {
		t.Fatal(err)
	}
	if !s.LastUpdate.Equal(old) {
		t.Errorf("LastUpdate = %v，要更旧的 %v", s.LastUpdate, old)
	}
}

// 两路混合时方向也混合：一个消耗型一个达成型，各自保留自己的方向，
// 否则经营数据会按额度口径配色（超前变成告警）。
func TestCompositeKeepsPerWindowDirection(t *testing.T) {
	c := NewComposite(
		NewClaude(usageAt(multiNow.Add(-time.Minute)), NewFixedTZ("UTC")),
		NewSales(salesAt(multiNow.Add(-2*time.Minute)), NewFixedTZ("UTC")),
	)
	s, err := c.Snapshot(context.Background(), "u1", multiNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Windows) != 2 {
		t.Fatalf("窗口数 = %d", len(s.Windows))
	}
	dirs := map[string]Direction{}
	for _, w := range s.Windows {
		dirs[w.Label] = w.Dir
	}
	if dirs["5 小时"] != Budget {
		t.Errorf("额度窗口方向 = %v，要 Budget", dirs["5 小时"])
	}
	if dirs["今日成交额"] != Target {
		t.Errorf("经营窗口方向 = %v，要 Target", dirs["今日成交额"])
	}
}

// 收到过数据但一个环都画不出来（经营数据的目标全是 0）不算一路：
// 另一路照样独占两个环。
func TestCompositeIgnoresSourceWithNoWindows(t *testing.T) {
	noGoal := fakeSalesStore{
		ok: true, received: multiNow.Add(-time.Minute),
		rep: sales.Report{
			Date: "2026-10-17", TZ: "UTC",
			GMV:    &sales.Metric{Value: 146_000, Goal: 0},
			Orders: &sales.Metric{Value: 420, Goal: 0},
		},
	}
	c := NewComposite(
		NewClaude(usageAt(multiNow.Add(-30*time.Minute)), NewFixedTZ("UTC")),
		NewSales(noGoal, NewFixedTZ("UTC")),
	)
	s, err := c.Snapshot(context.Background(), "u1", multiNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := labels(s); !eq(got, []string{"5 小时", "7 天"}) {
		t.Errorf("窗口 = %v，要额度独占两环", got)
	}
}

// 一个环都画不出来时，优先用「收到过数据」的那一路的快照，
// 让底栏的「更新 X 前」说得出话。
func TestCompositeNoWindowsPrefersSourceWithData(t *testing.T) {
	received := multiNow.Add(-time.Minute)
	noGoal := fakeSalesStore{
		ok: true, received: received,
		rep: sales.Report{Date: "2026-10-17", TZ: "UTC",
			GMV: &sales.Metric{Value: 1, Goal: 0}},
	}
	c := NewComposite(
		NewClaude(fakeUsageStore{}, NewFixedTZ("UTC")), // 从没上报过
		NewSales(noGoal, NewFixedTZ("UTC")),
	)
	s, err := c.Snapshot(context.Background(), "u1", multiNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Windows) != 0 {
		t.Fatalf("不该有窗口: %+v", s.Windows)
	}
	if !s.LastUpdate.Equal(received) {
		t.Errorf("LastUpdate = %v，要 %v", s.LastUpdate, received)
	}
}

// 一路数据都没有时要出占位快照，并且带着第一路的方向，让文案口径对得上。
func TestCompositeNoDataAtAll(t *testing.T) {
	c := NewComposite(
		NewClaude(fakeUsageStore{}, NewFixedTZ("UTC")),
		NewSales(fakeSalesStore{}, NewFixedTZ("UTC")),
	)
	s, err := c.Snapshot(context.Background(), "u1", multiNow)
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

// 一路挂了不该把另一路拖下水：剩下那路独占两个环。
func TestCompositeSurvivesOneFailure(t *testing.T) {
	boom := errors.New("数据库炸了")
	c := NewComposite(
		NewClaude(fakeUsageStore{err: boom}, NewFixedTZ("UTC")),
		NewSales(salesAt(multiNow.Add(-time.Minute)), NewFixedTZ("UTC")),
	)
	s, err := c.Snapshot(context.Background(), "u1", multiNow)
	if err != nil {
		t.Fatalf("一路失败不该整体报错: %v", err)
	}
	if got := labels(s); !eq(got, []string{"今日成交额", "今日订单"}) {
		t.Errorf("窗口 = %v，要经营数据独占两环", got)
	}
}

// 全挂了才往上报错——帧服务会保留上一张好帧。
func TestCompositeAllFail(t *testing.T) {
	boom := errors.New("数据库炸了")
	c := NewComposite(
		NewClaude(fakeUsageStore{err: boom}, NewFixedTZ("UTC")),
		NewSales(fakeSalesStoreErr{boom}, NewFixedTZ("UTC")),
	)
	if _, err := c.Snapshot(context.Background(), "u1", multiNow); !errors.Is(err, boom) {
		t.Fatalf("err = %v，要 %v", err, boom)
	}
}

type fakeSalesStoreErr struct{ err error }

func (f fakeSalesStoreErr) Latest(string) (sales.Report, time.Time, bool, error) {
	return sales.Report{}, time.Time{}, false, f.err
}

func TestCompositeID(t *testing.T) {
	c := NewComposite(NewClaude(fakeUsageStore{}, NewFixedTZ("UTC")), NewSales(fakeSalesStore{}, NewFixedTZ("UTC")))
	if got := c.ID(); got != "claude-usage+chaodai-sales" {
		t.Errorf("ID = %q", got)
	}
}
