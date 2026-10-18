package theme

import (
	"testing"
	"time"
)

func pl() Playlist {
	return Playlist{
		{ID: "quota", Ver: 7, Secs: 20},
		{ID: "sales", Ver: 3, Secs: 20},
	}
}

func TestPlaylistAccessors(t *testing.T) {
	p := pl()
	if primary, ok := p.Primary(); !ok || primary.ID != "quota" {
		t.Errorf("Primary = %+v", primary)
	}
	if _, ok := (Playlist{}).Primary(); ok {
		t.Error("空列表不该有主主题")
	}
	if s, ok := p.Find("sales"); !ok || s.Ver != 3 {
		t.Errorf("Find(sales) = %+v", s)
	}
	if _, ok := p.Find("nope"); ok {
		t.Error("不存在的主题不该找到")
	}
	if got := p.Total(); got != 40*time.Second {
		t.Errorf("Total = %v，要 40s", got)
	}
}

func TestPlaylistValidate(t *testing.T) {
	if err := pl().Validate(); err != nil {
		t.Fatalf("正常列表被拒: %v", err)
	}
	cases := map[string]Playlist{
		"空列表":    {},
		"缺主题 ID": {{ID: "", Ver: 1, Secs: 20}},
		"主题重复":   {{ID: "a", Ver: 1, Secs: 20}, {ID: "a", Ver: 2, Secs: 20}},
		// 版本 0 对设备是「没有帧」，不能当成一个可取的包下发。
		"版本为 0": {{ID: "a", Ver: 0, Secs: 20}},
		"停留太短":  {{ID: "a", Ver: 1, Secs: 1}},
		"停留太长":  {{ID: "a", Ver: 1, Secs: 3601}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if err := p.Validate(); err == nil {
				t.Error("应当被拒，但过了")
			}
		})
	}
}

func TestClampDwell(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0:                DefaultDwell,
		-time.Second:     DefaultDwell,
		time.Second:      MinDwell,
		30 * time.Second: 30 * time.Second,
		time.Hour:        MaxDwell,
	} {
		if got := ClampDwell(in); got != want {
			t.Errorf("ClampDwell(%v) = %v，要 %v", in, got, want)
		}
	}
}
