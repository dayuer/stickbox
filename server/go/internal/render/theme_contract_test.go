package render

import (
	"image"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/theme"
)

// checkThemeContract 是**任何**主题都要过的一组检查。
//
// 加新主题时只要挂进来，几何对齐、卡片尺寸、底帧留空这些共性问题就不用
// 每个主题重写一遍测试。这些错都不会在服务端炸，只会在电视上显示成错位
// 或者不跳的数字。
func checkThemeContract(t *testing.T, th theme.Theme) {
	t.Helper()
	if th.ID() == "" || th.Source() == "" {
		t.Errorf("主题缺 ID 或数据源: %q / %q", th.ID(), th.Source())
	}
	g := th.Geometry()
	if err := g.Validate(); err != nil {
		t.Fatalf("几何不合法: %v", err)
	}

	// 字形图集只有一种卡片尺寸，所以四个槽的宽高必须一致。
	w, h := g.Slots[0].W, g.Slots[0].H
	for i, s := range g.Slots {
		if s.W != w || s.H != h {
			t.Errorf("槽 %d 尺寸 %dx%d 与槽 0 的 %dx%d 不一致", i, s.W, s.H, w, h)
		}
	}
	for d := byte('0'); d <= '9'; d++ {
		b := th.CardTile(d).Bounds()
		if b.Dx() != w || b.Dy() != h {
			t.Fatalf("数字 %c 的卡片 %dx%d，要 %dx%d", d, b.Dx(), b.Dy(), w, h)
		}
	}

	snap := contractSnapshot()
	base, full := th.BaseImage(snap), th.FullImage(snap)
	for name, img := range map[string]*image.RGBA{"底帧": base, "整帧": full} {
		if b := img.Bounds(); b.Dx() != theme.Width || b.Dy() != theme.Height {
			t.Fatalf("%s 尺寸 %dx%d，要 %dx%d", name, b.Dx(), b.Dy(), theme.Width, theme.Height)
		}
	}

	// 底帧与整帧的差别**只能**在四张卡片里：设备每分钟换的就是那四块，
	// 别的地方不一样就说明画图用的坐标和 meta 告诉设备的坐标对不上，
	// 或者整帧模式多画了东西——两种都会让电视上出现错位/残影。
	inCard := func(x, y int) bool {
		for _, s := range g.Slots {
			if x >= s.X && x < s.X+s.W && y >= s.Y && y < s.Y+s.H {
				return true
			}
		}
		return false
	}
	diffOutside := 0
	for y := 0; y < theme.Height; y++ {
		for x := 0; x < theme.Width; x++ {
			if base.RGBAAt(x, y) != full.RGBAAt(x, y) && !inCard(x, y) {
				diffOutside++
			}
		}
	}
	if diffOutside != 0 {
		t.Errorf("底帧与整帧在卡片外有 %d 个像素不同，应当为 0", diffOutside)
	}

	// 卡片里：整帧的那一块必须**逐像素等于** CardTile——设备干的就是
	// 「把 CardTile 贴到槽位坐标上」，所以这是唯一精确的不变量。
	//
	// 别用「卡片内有差异」这种弱检查：数字比卡片窄，画歪几个像素仍然落在
	// 卡片矩形里，弱检查照样放过，而电视上已经错位了。（这一条是实测出来
	// 的：故意把画图坐标偏 8px，弱检查过了，逐像素检查和 frame 包的
	// TestSpliceMatchesFull 才抓住。）
	digits := clockDigits(snap.Now)
	for i, s := range g.Slots {
		tile := th.CardTile(digits[i])
		bad := 0
		for dy := 0; dy < s.H; dy++ {
			for dx := 0; dx < s.W; dx++ {
				if full.RGBAAt(s.X+dx, s.Y+dy) != tile.RGBAAt(dx, dy) {
					bad++
				}
			}
		}
		if bad != 0 {
			t.Errorf("槽 %d（x=%d，数字 %c）整帧与 CardTile 有 %d 个像素不同："+
				"画图坐标和几何对不上", i, s.X, digits[i], bad)
		}
	}

	// 底帧那一块必须和整帧不同，否则「留空给设备换字形」没生效，
	// 设备拼上去的数字会盖在一张已经画好数字的底帧上。
	for i, s := range g.Slots {
		same := true
		for dy := 0; dy < s.H && same; dy++ {
			for dx := 0; dx < s.W; dx++ {
				if base.RGBAAt(s.X+dx, s.Y+dy) != full.RGBAAt(s.X+dx, s.Y+dy) {
					same = false
					break
				}
			}
		}
		if same {
			t.Errorf("槽 %d 的底帧和整帧一样：底帧没给设备留空", i)
		}
	}
}

// clockDigits 是 HH:MM 四位，顺序和 Geometry().Slots 一致。
func clockDigits(t time.Time) [theme.NumSlots]byte {
	s := []byte(t.Format("1504"))
	return [theme.NumSlots]byte{s[0], s[1], s[2], s[3]}
}

func contractSnapshot() source.Snapshot {
	now := time.Date(2026, 10, 17, 12, 47, 0, 0, time.UTC)
	return source.Snapshot{
		Now:        now,
		LastUpdate: now.Add(-3 * time.Minute),
		DeviceIP:   "192.168.3.14",
		Windows: []source.Window{
			{Label: "5 小时", Length: 5 * time.Hour, UsedPercentage: .38,
				ResetsAt: now.Add(2 * time.Hour)},
			{Label: "7 天", Length: 7 * 24 * time.Hour, UsedPercentage: .20,
				ResetsAt: now.Add(100 * time.Hour)},
		},
	}
}

func TestQuotaThemeContract(t *testing.T) {
	rr, err := New()
	if err != nil {
		t.Fatal(err)
	}
	checkThemeContract(t, Quota{R: rr})
}
