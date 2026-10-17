package sim

import (
	"bytes"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/frame"
	"github.com/dayuer/stickbox/server/go/internal/frames"
	"github.com/dayuer/stickbox/server/go/internal/render"
	"github.com/dayuer/stickbox/server/go/internal/source"
)

// 用真实渲染器出一帧，验证模拟器的逐行拼接与 frame.Bundle.Splice 逐字节一致。
func realBundle(t *testing.T) *frame.Bundle {
	t.Helper()
	rr, err := render.New()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 17, 12, 47, 0, 0, time.FixedZone("CST", 8*3600))
	snap := source.Snapshot{
		Now: now,
		Windows: []source.Window{
			{Label: "5 小时", Length: 5 * time.Hour, UsedPercentage: 0.42, ResetsAt: now.Add(3 * time.Hour)},
			{Label: "7 天", Length: 7 * 24 * time.Hour, UsedPercentage: 0.34, ResetsAt: now.Add(4 * 24 * time.Hour)},
		},
		LastUpdate: now.Add(-2 * time.Minute),
	}
	b, err := frame.Build(render.Quota{R: rr}, snap)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParsePackAndSpliceMatchesServer(t *testing.T) {
	b := realBundle(t)
	pack, err := frames.Pack(b)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := ParsePack(pack)
	if err != nil {
		t.Fatalf("ParsePack: %v", err)
	}
	if sb.Meta.Band.MaxBandBytes > BandCap {
		t.Fatalf("max_band_bytes = %d", sb.Meta.Band.MaxBandBytes)
	}

	for _, hm := range [][2]int{{0, 0}, {9, 5}, {8, 28}, {12, 47}, {23, 59}} {
		got, err := Splice(sb, hm[0], hm[1])
		if err != nil {
			t.Fatalf("Splice(%02d:%02d): %v", hm[0], hm[1], err)
		}
		want, err := b.Splice(hm[0], hm[1])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%02d:%02d 拼接结果与服务端不一致：%d vs %d 字节", hm[0], hm[1], len(got), len(want))
		}
	}
}

func TestParsePackRejectsBandCap(t *testing.T) {
	b := realBundle(t)
	b.Meta.Band.MaxBandBytes = BandCap + 1
	pack, err := frames.Pack(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePack(pack); err == nil {
		t.Fatal("max_band_bytes 超限仍被接受")
	}
}

func TestParsePackRejectsHashMismatch(t *testing.T) {
	b := realBundle(t)
	pack, err := frames.Pack(b)
	if err != nil {
		t.Fatal(err)
	}
	pack[16] ^= 0xff // 改底帧第一个字节
	if _, err := ParsePack(pack); err == nil {
		t.Fatal("底帧被改后仍被接受")
	}
}

func TestSpliceRejectsBadTime(t *testing.T) {
	sb := &Bundle{Meta: frame.Meta{}}
	if _, err := Splice(sb, 24, 0); err == nil {
		t.Error("24:00 应该被拒")
	}
	if _, err := Splice(sb, 0, 60); err == nil {
		t.Error("00:60 应该被拒")
	}
}
