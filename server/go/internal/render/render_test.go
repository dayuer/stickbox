package render

import (
	"bytes"
	"flag"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/source"
)

var update = flag.Bool("update", false, "重新生成黄金图")

var testNow = time.Date(2026, 10, 17, 12, 47, 0, 0, time.FixedZone("CST", 8*3600))

type preview struct {
	name string
	has  bool
	u5   float64
	rem5 time.Duration
	u7   float64
	rem7 time.Duration
}

var previews = []preview{
	{"01-超速", true, .02, 4*time.Hour + 15*time.Minute + 30*time.Second, .34, 4*24*time.Hour + 18*time.Hour + 13*time.Minute},
	{"02-安全", true, .38, 2*time.Hour + 24*time.Minute + 2*time.Second, .20, 4*24*time.Hour + 18*time.Hour + 13*time.Minute},
	{"03-危险", true, .92, time.Hour + 4*time.Minute + 55*time.Second, .34, 4*24*time.Hour + 18*time.Hour + 13*time.Minute},
	{"04-用满", true, 1, 42*time.Minute + 15*time.Second, .364, 4*24*time.Hour + 16*time.Hour},
	{"05-无数据", false, 0, 0, 0, 0},
}

func snapshot(p preview) source.Snapshot {
	s := source.Snapshot{Now: testNow, LastUpdate: testNow.Add(-3 * time.Minute), DeviceIP: "192.168.3.14"}
	if !p.has {
		return s
	}
	s.Windows = []source.Window{
		{Label: "5 小时", Length: 5 * time.Hour, UsedPercentage: p.u5, ResetsAt: testNow.Add(p.rem5)},
		{Label: "7 天", Length: 7 * 24 * time.Hour, UsedPercentage: p.u7, ResetsAt: testNow.Add(p.rem7)},
	}
	return s
}

// TestGoldenFrames 是黄金图回归。像素/字体的任何变化都会让字节不同。
// 有意改版面时用 `go test ./internal/render -update` 重新生成。
func TestGoldenFrames(t *testing.T) {
	rr, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range previews {
		frame, err := EncodeFrame(Quota{R: rr}.FullImage(snapshot(p)))
		if err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		if len(frame) > TargetFrameBytes {
			t.Errorf("%s: %d 字节，超过目标 %d", p.name, len(frame), TargetFrameBytes)
		}
		if len(frame) > HardMaxFrameBytes {
			t.Fatalf("%s: %d 字节，超过硬上限 %d", p.name, len(frame), HardMaxFrameBytes)
		}
		path := filepath.Join("testdata", p.name+".jpg")
		if *update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, frame, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: 读黄金图失败（先跑 -update）: %v", p.name, err)
		}
		if !bytes.Equal(frame, want) {
			got := filepath.Join("testdata", p.name+".got.jpg")
			_ = os.WriteFile(got, frame, 0o644)
			t.Errorf("%s: 渲染结果与黄金图不同（实际见 %s）", p.name, got)
		}
	}
}

func TestWindowStates(t *testing.T) {
	tests := []struct {
		name      string
		scenario  int
		mainLabel string
		mainRank  int
		mainRem   string
		mainPred  string
		secLabel  string
		headline  string
		stats     []string
	}{
		{"超速", 0, "7 天", 2, "剩 4天18时", "约4天8时用完", "5 小时", "7 天额度用得偏快",
			[]string{"34%", "32%", "快 2%", "13.9%/天"}},
		{"安全", 1, "5 小时", 1, "剩 2时24分02秒", "够用到重置", "7 天", "两个窗口都够用",
			[]string{"38%", "52%", "慢 14%", "25.8%/时"}},
		{"危险", 2, "5 小时", 3, "剩 1时04分55秒", "约20分用完", "7 天", "5 小时额度快用完了",
			[]string{"92%", "78%", "快 14%", "7.4%/时"}},
		{"用满", 3, "5 小时", 4, "剩 42分15秒", "已用满", "7 天", "5 小时额度已用满",
			[]string{"100%", "86%", "快 14%", "--"}},
		{"无数据", 4, "5 小时", 0, "", "等待数据", "7 天", "等待电脑上报数据",
			[]string{"--", "--", "--", "--"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			main, sec := views(snapshot(previews[tc.scenario]))
			if main.label != tc.mainLabel {
				t.Errorf("主窗口 = %q，要 %q", main.label, tc.mainLabel)
			}
			if main.rank != tc.mainRank {
				t.Errorf("主窗口 rank = %d，要 %d", main.rank, tc.mainRank)
			}
			if main.remT != tc.mainRem {
				t.Errorf("主窗口剩余 = %q，要 %q", main.remT, tc.mainRem)
			}
			if main.pred != tc.mainPred {
				t.Errorf("主窗口预测 = %q，要 %q", main.pred, tc.mainPred)
			}
			if sec.label != tc.secLabel {
				t.Errorf("次窗口 = %q，要 %q", sec.label, tc.secLabel)
			}
			if got, _ := headline(main); got != tc.headline {
				t.Errorf("结论句 = %q，要 %q", got, tc.headline)
			}
			st := stats(main)
			for i, want := range tc.stats {
				if st[i].value != want {
					t.Errorf("统计 %d = %q，要 %q", i, st[i].value, want)
				}
			}
		})
	}
}

func BenchmarkRender(b *testing.B) {
	rr, err := New()
	if err != nil {
		b.Fatal(err)
	}
	s := snapshot(previews[0])
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := EncodeFrame(Quota{R: rr}.FullImage(s)); err != nil {
			b.Fatal(err)
		}
	}
}

func TestGoldenFrameIsBaselineJPEG(t *testing.T) {
	rr, err := New()
	if err != nil {
		t.Fatal(err)
	}
	frame, err := EncodeFrame(Quota{R: rr}.FullImage(snapshot(previews[0])))
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("不是可解码的 JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 1920 || b.Dy() != 1080 {
		t.Fatalf("尺寸 = %v，要 1920×1080", b)
	}
}
