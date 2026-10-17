package raster

import (
	"encoding/binary"
	"testing"
)

// 生成的测试页必须能过自己的入口校验。
//
// 服务端拿它当普通作业入队，走的是同一条 Verify 路径——过不了的话，
// 用户点「开始测试」会看到一个来自服务端自己的 400。
func TestTestPagePassesVerify(t *testing.T) {
	b := TestPage(4962, 7014, 600, KindMark)
	if len(b) == 0 {
		t.Fatal("没生成东西")
	}
	info, err := Verify(FormatURF, b)
	if err != nil {
		t.Fatalf("自己生成的页过不了自己的校验：%v", err)
	}
	if info.Pages != 1 {
		t.Errorf("页数 = %d，期望 1", info.Pages)
	}
	if info.Width != 4962 || info.Height != 7014 {
		t.Errorf("尺寸 = %dx%d，期望 4962x7014", info.Width, info.Height)
	}
}

// 页数字段必须非 0——写 0 打印机认为文档为空，什么都不打，
// 而这正是「点了测试没动静」最难查的那种症状。
func TestTestPageHasNonZeroPageCount(t *testing.T) {
	b := TestPage(1240, 1754, 300, KindMark)
	if got := binary.BigEndian.Uint32(b[8:12]); got != 1 {
		t.Errorf("页数字段 = %d，必须非 0", got)
	}
}

// 行数据要完整覆盖页高，否则打印机解码到一半就停。
func TestTestPageRowsCoverFullHeight(t *testing.T) {
	const w, h = 600, 400
	b := TestPage(w, h, 300, KindMargins)

	pos := 12 + 32
	rows := 0
	for rows < h && pos < len(b) {
		repeat := int(b[pos])
		pos++
		px := 0
		for px < w && pos < len(b) {
			n := int(b[pos])
			pos++
			switch {
			case n < 128:
				px += n + 1
				pos++
			case n == 128:
				t.Fatal("128 不是合法的包首字节")
			default:
				cnt := 257 - n
				px += cnt
				pos += cnt
			}
		}
		if px != w {
			t.Fatalf("第 %d 条记录的像素数 %d != 页宽 %d", rows, px, w)
		}
		rows += repeat + 1
	}
	if rows != h {
		t.Errorf("行数据覆盖 %d 行，页高是 %d", rows, h)
	}
	if pos != len(b) {
		t.Errorf("解析停在 %d，文件长 %d——尾部有多余字节", pos, len(b))
	}
}

// decodeInk 解开产物，数有多少个非白像素。
//
// 直接数墨点，不用「字节数比空白页大」那种间接判据——那种判据我一开始就
// 写错了：拿一个不存在的 kind 当空白基线，而 default 分支画的恰好就是
// KindMark，两者当然一样大。
func decodeInk(t *testing.T, b []byte, w, h int) int {
	t.Helper()
	pos, rows, ink := 12+32, 0, 0
	for rows < h && pos < len(b) {
		repeat := int(b[pos]) + 1
		pos++
		rowInk, px := 0, 0
		for px < w && pos < len(b) {
			n := int(b[pos])
			pos++
			if n < 128 {
				cnt := n + 1
				if b[pos] != white {
					rowInk += cnt
				}
				px += cnt
				pos++
			} else {
				cnt := 257 - n
				for i := 0; i < cnt; i++ {
					if b[pos+i] != white {
						rowInk++
					}
				}
				px += cnt
				pos += cnt
			}
		}
		ink += rowInk * repeat
		rows += repeat
	}
	return ink
}

// 图案要真的画上去，不能是一张白纸。
//
// 坐标写错时页数、尺寸、校验全都照样通过，而症状就是「打了但什么都没有」
// ——用户没法把它和「没打印」区分开，那正是这次「点了没动静」的翻版。
func TestTestPageHasInk(t *testing.T) {
	const w, h = 1240, 1754
	for _, kind := range []TestPageKind{KindMark, KindMargins} {
		ink := decodeInk(t, TestPage(w, h, 300, kind), w, h)
		if ink == 0 {
			t.Errorf("%s 一个墨点都没有——图案没画上去", kind)
		}
		// 也不能糊满整页：那是「整页黑」，比白纸更糟（费墨还看不出边距）。
		if frac := float64(ink) / float64(w*h); frac > 0.35 {
			t.Errorf("%s 覆盖了 %.0f%% 的页面，太多了", kind, frac*100)
		}
	}
}

// 边距页的墨要贴在四条边上——量的就是它到纸边的距离。
func TestMarginsPageDrawsBorder(t *testing.T) {
	const w, h = 1240, 1754
	b := TestPage(w, h, 300, KindMargins)
	// 第一行必须是整行黑，否则量不到上边距。
	pos := 12 + 32
	repeat := int(b[pos]) + 1
	_ = repeat
	pos++
	n := int(b[pos])
	if n >= 128 {
		t.Fatalf("首行不是游程编码，首字节 %d", n)
	}
	if b[pos+1] != black {
		t.Errorf("首行不是黑的（%#x）——上边距量不了", b[pos+1])
	}
}

// 页面变高不该让重复计数溢出：一个字节最多表示 256 行。
func TestTestPageHandlesTallPages(t *testing.T) {
	b := TestPage(600, 5000, 300, KindMark)
	if _, err := Verify(FormatURF, b); err != nil {
		t.Fatalf("高页面产物非法：%v", err)
	}
}
