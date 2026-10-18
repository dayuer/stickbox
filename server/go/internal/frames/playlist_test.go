package frames

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/sales"
	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/theme"
)

// fakeSales 喂第二个主题那一路数据。
type fakeSales struct {
	rep sales.Report
	at  time.Time
	ok  bool
}

func (f *fakeSales) Latest(string) (sales.Report, time.Time, bool, error) {
	return f.rep, f.at, f.ok, nil
}

func salesReport() sales.Report {
	return sales.Report{
		Date: "2026-10-17", TZ: "UTC",
		GMV:    &sales.Metric{Value: 146_000, Goal: 200_000},
		Orders: &sales.Metric{Value: 420, Goal: 400},
	}
}

// twoThemes 建一个两主题的服务：quota 吃 Claude，sales 吃经营数据。
func twoThemes(t *testing.T, f *fixture, sl *fakeSales) *Service {
	t.Helper()
	themes := []Themed{
		{Theme: stubTheme{id: "quota", src: "claude-usage"},
			Source: source.NewClaude(f.us, "UTC")},
		{Theme: stubTheme{id: "sales", src: "chaodai-sales"},
			Source: source.NewSales(sl, "UTC")},
	}
	svc, err := New(f.root, f.st, themes, f.pub, f.b, Options{
		Debounce: 5 * time.Millisecond, RenderConcurrency: 1,
		Dwell: 30 * time.Second, Now: f.currentTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return svc
}

func lastPlaylist(t *testing.T, f *fixture, dev string) (uint64, theme.Playlist) {
	t.Helper()
	f.pub.mu.Lock()
	defer f.pub.mu.Unlock()
	msgs := f.pub.sent[dev]
	if len(msgs) == 0 {
		t.Fatalf("%s 没收到帧通知", dev)
	}
	var got struct {
		Ver uint64         `json:"ver"`
		PL  theme.Playlist `json:"pl"`
	}
	if err := json.Unmarshal(msgs[len(msgs)-1], &got); err != nil {
		t.Fatal(err)
	}
	return got.Ver, got.PL
}

// 两个主题各存一份帧包，播放列表按注册顺序列出，各带自己的版本。
func TestPlaylistTwoThemes(t *testing.T) {
	f := newFixture(t)
	f.svc.Close() // 换成两主题的服务
	sl := &fakeSales{ok: true, rep: salesReport(), at: f.currentTime().Add(-time.Minute)}
	svc := twoThemes(t, f, sl)

	if err := svc.Rebuild("u1"); err != nil {
		t.Fatal(err)
	}
	pl := svc.Playlist("u1")
	if len(pl) != 2 {
		t.Fatalf("播放列表 = %+v，要两项", pl)
	}
	if pl[0].ID != "quota" || pl[1].ID != "sales" {
		t.Errorf("顺序 = %q,%q，要按注册顺序 quota,sales", pl[0].ID, pl[1].ID)
	}
	for _, s := range pl {
		if s.Ver != 1 {
			t.Errorf("%s 首个版本 = %d，要 1", s.ID, s.Ver)
		}
		if s.Secs != 30 {
			t.Errorf("%s 停留 = %ds，要 30", s.ID, s.Secs)
		}
	}
	if err := pl.Validate(); err != nil {
		t.Errorf("下发前校验不过: %v", err)
	}

	// 两个主题的帧包必须是两份不同的字节，不能互相覆盖。
	q, qv, ok := svc.Current("u1", "quota")
	if !ok {
		t.Fatal("取不到 quota 的帧包")
	}
	sa, sv, ok := svc.Current("u1", "sales")
	if !ok {
		t.Fatal("取不到 sales 的帧包")
	}
	if string(q) == string(sa) {
		t.Error("两个主题存成了同一份帧包")
	}
	if qv != 1 || sv != 1 {
		t.Errorf("版本 = %d/%d", qv, sv)
	}
}

// 各主题的版本各自递增：只有一路数据变了，不该把另一路的版本也推上去
// ——设备拿版本号做等值比较，白推一次就是一次没必要的 256KB 下载。
func TestPlaylistVersionsAreIndependent(t *testing.T) {
	f := newFixture(t)
	f.svc.Close()
	sl := &fakeSales{ok: true, rep: salesReport(), at: f.currentTime().Add(-time.Minute)}
	svc := twoThemes(t, f, sl)

	if err := svc.Rebuild("u1"); err != nil {
		t.Fatal(err)
	}
	// 只有经营数据变了。
	f.setNow(f.currentTime().Add(time.Minute))
	r := salesReport()
	r.GMV.Value = 180_000
	sl.rep, sl.at = r, f.currentTime()
	if err := svc.Rebuild("u1"); err != nil {
		t.Fatal(err)
	}

	pl := svc.Playlist("u1")
	q, _ := pl.Find("quota")
	sa, _ := pl.Find("sales")
	if sa.Ver != 2 {
		t.Errorf("sales 版本 = %d，要 2", sa.Ver)
	}
	// quota 这一路的数据没变，但 Rebuild 是整轮的，所以它也会重渲一次。
	// 这里只钉住「两个版本相互独立地计数」，不钉具体值。
	if q.Ver == 0 {
		t.Error("quota 版本不该为 0")
	}
}

// 通知里 pl 是新固件读的，ver 单独留给老固件，必须等于主主题的版本。
func TestPlaylistPayloadKeepsLegacyVer(t *testing.T) {
	f := newFixture(t)
	f.svc.Close()
	sl := &fakeSales{ok: true, rep: salesReport(), at: f.currentTime().Add(-time.Minute)}
	svc := twoThemes(t, f, sl)

	if err := svc.Rebuild("u1"); err != nil {
		t.Fatal(err)
	}
	ver, pl := lastPlaylist(t, f, "d1")
	if len(pl) != 2 {
		t.Fatalf("通知里的播放列表 = %+v", pl)
	}
	primary, _ := pl.Primary()
	if primary.ID != "quota" {
		t.Errorf("主主题 = %q", primary.ID)
	}
	if ver != primary.Ver {
		t.Errorf("兼容字段 ver = %d，要等于主主题版本 %d", ver, primary.Ver)
	}
}

// 非主主题那一路还没数据时不进轮播：否则只报了一路的用户会看到一块
// 空白的「等待上报」看板轮流上屏。
func TestPlaylistSkipsThemeWithoutData(t *testing.T) {
	f := newFixture(t)
	f.svc.Close()
	sl := &fakeSales{} // 从没上报过
	svc := twoThemes(t, f, sl)

	if err := svc.Rebuild("u1"); err != nil {
		t.Fatal(err)
	}
	pl := svc.Playlist("u1")
	if len(pl) != 1 || pl[0].ID != "quota" {
		t.Fatalf("播放列表 = %+v，只该有主主题", pl)
	}
	if _, _, ok := svc.Current("u1", "sales"); ok {
		t.Error("没数据的主题不该出帧包")
	}

	// 经营数据来了，它才加入轮播。
	f.setNow(f.currentTime().Add(time.Minute))
	sl.ok, sl.rep, sl.at = true, salesReport(), f.currentTime()
	if err := svc.Rebuild("u1"); err != nil {
		t.Fatal(err)
	}
	if pl := svc.Playlist("u1"); len(pl) != 2 {
		t.Fatalf("有数据后播放列表 = %+v，要两项", pl)
	}
}

// 主主题即使没有数据也要出帧：新配对的设备总得有「等待上报」那张帧可显示。
func TestPlaylistPrimaryAlwaysBuilds(t *testing.T) {
	f := newFixture(t)
	f.svc.Close()
	f.us.ok = false // Claude 那一路也没上报过
	sl := &fakeSales{}
	svc := twoThemes(t, f, sl)

	if err := svc.Rebuild("u1"); err != nil {
		t.Fatal(err)
	}
	pl := svc.Playlist("u1")
	if len(pl) != 1 || pl[0].ID != "quota" {
		t.Fatalf("播放列表 = %+v，要有主主题的等待帧", pl)
	}
}

// 取帧端点按主题取；不认识的主题要能分辨出来，不能当成「还没出帧」。
func TestCurrentForKeyByTheme(t *testing.T) {
	f := newFixture(t)
	f.svc.Close()
	sl := &fakeSales{ok: true, rep: salesReport(), at: f.currentTime().Add(-time.Minute)}
	svc := twoThemes(t, f, sl)

	if err := svc.Rebuild("u1"); err != nil {
		t.Fatal(err)
	}
	// 不带主题名 = 老固件，给主主题。
	bare, _, err := svc.CurrentForKey("k1", "d1", "")
	if err != nil {
		t.Fatal(err)
	}
	q, _, err := svc.CurrentForKey("k1", "d1", "quota")
	if err != nil {
		t.Fatal(err)
	}
	if string(bare) != string(q) {
		t.Error("不带主题名应当等于主主题")
	}
	sa, _, err := svc.CurrentForKey("k1", "d1", "sales")
	if err != nil {
		t.Fatal(err)
	}
	if string(sa) == string(q) {
		t.Error("两个主题返回了同一份帧包")
	}
	if _, _, err := svc.CurrentForKey("k1", "d1", "nope"); err != ErrNoTheme {
		t.Errorf("未知主题 err = %v，要 ErrNoTheme", err)
	}
}

// ETag 必须带主题名：两个主题都在 v1 时，只用版本号会让切主题的设备
// 收到假 304，屏幕上就一直是上一个主题的画面。
func TestETagDistinguishesThemes(t *testing.T) {
	f := newFixture(t)
	f.svc.Close()
	sl := &fakeSales{ok: true, rep: salesReport(), at: f.currentTime().Add(-time.Minute)}
	svc := twoThemes(t, f, sl)

	if svc.ETag("quota", 1) == svc.ETag("sales", 1) {
		t.Fatal("同版本不同主题的 ETag 相同")
	}
	if svc.ETag("", 1) != svc.ETag("quota", 1) {
		t.Error("空主题名应当等于主主题")
	}
}

// 解绑后这个用户的**每一个**主题都要清掉，不能只清主主题。
func TestDropRemovesAllThemes(t *testing.T) {
	f := newFixture(t)
	f.svc.Close()
	sl := &fakeSales{ok: true, rep: salesReport(), at: f.currentTime().Add(-time.Minute)}
	svc := twoThemes(t, f, sl)

	if err := svc.Rebuild("u1"); err != nil {
		t.Fatal(err)
	}
	if len(svc.Playlist("u1")) != 2 {
		t.Fatal("前置条件：该有两个主题")
	}
	f.st.devs["u1"] = nil
	svc.DeviceRemoved("u1")

	if pl := svc.Playlist("u1"); len(pl) != 0 {
		t.Errorf("解绑后播放列表 = %+v，要空", pl)
	}
	for _, id := range []string{"quota", "sales"} {
		if _, _, ok := svc.Current("u1", id); ok {
			t.Errorf("%s 的帧包没清掉", id)
		}
	}
}
