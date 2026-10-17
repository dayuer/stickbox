package frame

import (
	"bytes"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/jpegrestart"
	"github.com/dayuer/stickbox/server/go/internal/render"
	"github.com/dayuer/stickbox/server/go/internal/source"
)

var testNow = time.Date(2026, 10, 17, 12, 47, 0, 0, time.FixedZone("CST", 8*3600))

type state struct {
	name string
	has  bool
	u5   float64
	rem5 time.Duration
	u7   float64
	rem7 time.Duration
}

var states = []state{
	{"超速", true, .02, 4*time.Hour + 15*time.Minute + 30*time.Second, .34, 4*24*time.Hour + 18*time.Hour + 13*time.Minute},
	{"安全", true, .38, 2*time.Hour + 24*time.Minute + 2*time.Second, .20, 4*24*time.Hour + 18*time.Hour + 13*time.Minute},
	{"危险", true, .92, time.Hour + 4*time.Minute + 55*time.Second, .34, 4*24*time.Hour + 18*time.Hour + 13*time.Minute},
	{"用满", true, 1, 42*time.Minute + 15*time.Second, .364, 4*24*time.Hour + 16*time.Hour},
	{"无数据", false, 0, 0, 0, 0},
}

func snapshot(st state, now time.Time) source.Snapshot {
	s := source.Snapshot{Now: now, LastUpdate: now.Add(-3 * time.Minute), DeviceIP: "192.168.3.14"}
	if !st.has {
		return s
	}
	s.Windows = []source.Window{
		{Label: "5 小时", Length: 5 * time.Hour, UsedPercentage: st.u5, ResetsAt: now.Add(st.rem5)},
		{Label: "7 天", Length: 7 * 24 * time.Hour, UsedPercentage: st.u7, ResetsAt: now.Add(st.rem7)},
	}
	return s
}

func TestSpliceMatchesFull(t *testing.T) {
	rr, err := render.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			snap := snapshot(st, testNow)
			b, err := Build(rr, snap)
			if err != nil {
				t.Fatal(err)
			}
			spliced, err := b.Splice(12, 47)
			if err != nil {
				t.Fatal(err)
			}
			full, err := jpegrestart.Encode(rr.FullImage(snap), jpegrestart.Options{
				Quality: Quality, RestartInterval: DRI,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := decodeJPEG(t, spliced)
			want := decodeJPEG(t, full.JPEG)
			if diff := maxDiff(got, want, image.Rect(0, 0, Width, Height)); diff != 0 {
				t.Errorf("拼接帧与整帧有 %d 的最大像素差（应为 0）", diff)
			}
		})
	}
}

// 任意 HH:MM 只拼时钟带：和该时刻直接渲染的时钟带逐像素一致。
func TestSpliceArbitraryClock(t *testing.T) {
	rr, err := render.New()
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshot(states[0], testNow)
	b, err := Build(rr, snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, hm := range [][2]int{{0, 0}, {9, 5}, {12, 47}, {23, 59}} {
		spliced, err := b.Splice(hm[0], hm[1])
		if err != nil {
			t.Fatalf("%02d:%02d: %v", hm[0], hm[1], err)
		}
		got := decodeJPEG(t, spliced)

		clockNow := time.Date(2026, 10, 17, hm[0], hm[1], 0, 0, testNow.Location())
		want := decodeJPEG(t, encodeFull(t, rr, snapshot(states[0], clockNow)))
		band := image.Rect(0, render.ClockBandRow0*16, Width, render.ClockBandRow1*16)
		if diff := maxDiff(got, want, band); diff != 0 {
			t.Errorf("%02d:%02d: 时钟带最大像素差 %d", hm[0], hm[1], diff)
		}
	}
}

func TestBundleFitsSlot(t *testing.T) {
	rr, err := render.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range states {
		b, err := Build(rr, snapshot(st, testNow))
		if err != nil {
			t.Fatal(err)
		}
		meta, err := b.MetaJSON()
		if err != nil {
			t.Fatal(err)
		}
		total := len(b.Base) + len(b.Atlas)
		var rowSum int
		for _, r := range b.Meta.Band.RowRuns {
			rowSum += r.Len
		}
		t.Logf("%s: 底帧 %d B + 图集 %d B = %d B；元数据 JSON %d B；合计 %d B；整带 %d B；12 行时钟列 %d B",
			st.name, len(b.Base), len(b.Atlas), total, len(meta), total+len(meta),
			b.Meta.Band.Run.Len, rowSum)
		// 每行卡片区替换成「各槽最长数字段」后的最坏长度（双缓冲的一半）。
		worstRows := 0
		for row := 0; row < CardRows; row++ {
			n := b.Meta.Band.RowRuns[row].Len
			for slot := 0; slot < 4; slot++ {
				baseLen := b.Meta.Slots[slot].Runs[row].Len
				maxLen := 0
				for d := 0; d < 10; d++ {
					if l := b.Meta.Atlas.Digits[d].Runs[row].Len; l > maxLen {
						maxLen = l
					}
				}
				n += maxLen - baseLen
			}
			worstRows += n
		}
		t.Logf("        逐行卡片区上界 %d B；遍历 1440 个时刻的实际最坏 %d B；BAND_CAP 16KB",
			worstRows, b.Meta.Band.MaxBandBytes)
		if worstRows > BandCap {
			t.Errorf("%s: 逐行卡片区上界 %d B 超过 BandCap %d", st.name, worstRows, BandCap)
		}
		if b.Meta.Band.MaxBandBytes > BandCap {
			t.Errorf("%s: max_band_bytes %d 超过 BandCap %d", st.name, b.Meta.Band.MaxBandBytes, BandCap)
		}
		if b.Meta.Band.MaxBandBytes != 13089 {
			t.Logf("%s: max_band_bytes = %d（08c769e 的基准是 13089，字体/质量变化时会变）",
				st.name, b.Meta.Band.MaxBandBytes)
		}
		if total+len(meta) > 256<<10 {
			t.Errorf("%s: 帧包 %d B 超过 256KB 槽位", st.name, total+len(meta))
		}
	}
}

func TestSpliceDecodesWithLibjpeg(t *testing.T) {
	if _, err := exec.LookPath("djpeg"); err != nil {
		t.Skip("没有 djpeg")
	}
	rr, err := render.New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(rr, snapshot(states[0], testNow))
	if err != nil {
		t.Fatal(err)
	}
	for _, hm := range [][2]int{{0, 0}, {9, 5}, {12, 47}, {23, 59}} {
		spliced, err := b.Splice(hm[0], hm[1])
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		in := filepath.Join(dir, "frame.jpg")
		out := filepath.Join(dir, "frame.ppm")
		if err := os.WriteFile(in, spliced, 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("djpeg", "-outfile", out, in)
		if msg, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%02d:%02d: djpeg 失败: %v\n%s", hm[0], hm[1], err, msg)
		}
		if st, err := os.Stat(out); err != nil || st.Size() == 0 {
			t.Fatalf("%02d:%02d: djpeg 没有产出", hm[0], hm[1])
		}
	}
}

func TestValidateBandSize(t *testing.T) {
	if err := validateBandSize(BandCap); err != nil {
		t.Errorf("恰好等于上限时不应失败: %v", err)
	}
	if err := validateBandSize(BandCap + 1); err == nil {
		t.Errorf("超过上限时必须失败")
	}
	if BandCap != 16<<10 {
		t.Errorf("BandCap = %d，必须与 rom/main/stream.h 的 BAND_CAP=16KB 同步", BandCap)
	}
}

func encodeFull(t *testing.T, rr *render.Renderer, s source.Snapshot) []byte {
	t.Helper()
	res, err := jpegrestart.Encode(rr.FullImage(s), jpegrestart.Options{
		Quality: Quality, RestartInterval: DRI,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.JPEG
}

func decodeJPEG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	return img
}

func maxDiff(a, b image.Image, r image.Rectangle) int {
	worst := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			r1, g1, b1, _ := a.At(x, y).RGBA()
			r2, g2, b2, _ := b.At(x, y).RGBA()
			for _, d := range []int{
				abs(int(r1>>8) - int(r2>>8)),
				abs(int(g1>>8) - int(g2>>8)),
				abs(int(b1>>8) - int(b2>>8)),
			} {
				if d > worst {
					worst = d
				}
			}
		}
	}
	return worst
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
