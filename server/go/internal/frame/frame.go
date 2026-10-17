// Package frame 打包「底帧 JPEG + 数字字形图集 + 索引」，
// 并在主机侧实现 T13b 方案 D 的拼接。
//
// 帧用 DRI=8（每 8 个 MCU 一个 restart 区间）编码。四张时钟卡片
// 各占 8 个 MCU 宽、12 行，正好是每一行完整的一个 restart 区间。
// 设备每分钟只需把对应卡片的 12 段字节换成字形段（RSTn 序号按目标
// 位置提前算好/补一个字节），不跑 DCT。
package frame

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dayuer/stickbox/server/go/internal/jpegrestart"
	"github.com/dayuer/stickbox/server/go/internal/render"
	"github.com/dayuer/stickbox/server/go/internal/source"
)

const (
	Version         = 1
	Width           = 1920
	Height          = 1080
	MCUCols         = render.ClockMCUCols
	MCURows         = (Height + 15) / 16
	DRI             = render.ClockDRI
	CardRows        = render.ClockCardRows
	intervalsPerRow = MCUCols / DRI
	// Quality 是底帧与字形图集的 JPEG 质量。q=82 时底帧约 181KB、
	// 图集约 32KB，连同索引仍在 256KB 槽位内；再高会顶到槽位上限。
	Quality = 82

	// BandCap 是设备侧逐行时钟缓冲（单块）的上限。必须与
	// rom/main/stream.h:10 的 BAND_CAP 保持一致；Build 在这里先硬失败，
	// 避免服务端出了设备装不下的帧包、到板上才拒包。
	//
	// 注意合并顺序：设备侧当前仍是 T7 的 12288。T13b 把它改成 16KB、
	// 并加上「加载时校验 + 拼接断言」之前，不要把 T13a 的帧包下发给设备。
	BandCap = 16 << 10
)

// Run 是文件/图集里的一段字节。
type Run struct {
	Off int `json:"off"`
	Len int `json:"len"`
}

// Slot 是一张数字卡片在底帧里的位置与 12 段字节。
type Slot struct {
	X      int           `json:"x"`
	Y      int           `json:"y"`
	W      int           `json:"w"`
	H      int           `json:"h"`
	MCUCol int           `json:"mcu_col"`
	Runs   [CardRows]Run `json:"runs"`
}

// Digit 是一个数字在字形图集里的 12 段字节（相对图集起点）。
type Digit struct {
	D    int           `json:"d"`
	Runs [CardRows]Run `json:"runs"`
}

// Meta 是随帧下发的元数据。JSON 里的 Atlas.Data 由 encoding/json
// 自动编码成 base64。
type Meta struct {
	V      int    `json:"v"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Frame  string `json:"frame_sha256"`
	JPEG   struct {
		DRI     int `json:"dri"`
		MCUCols int `json:"mcu_cols"`
		MCURows int `json:"mcu_rows"`
	} `json:"jpeg"`
	Band struct {
		Rows    [2]int        `json:"rows"`
		Run     Run           `json:"run"`
		RowRuns [CardRows]Run `json:"row_runs"`
		// MaxBandBytes 是遍历 00:00–23:59 后，12 行卡片区替换完的最大字节数。
		// 设备加载帧包时拿它和 BAND_CAP 比对。
		MaxBandBytes int `json:"max_band_bytes"`
	} `json:"band"`
	Slots [4]Slot `json:"slots"`
	Atlas struct {
		Size   int       `json:"size"`
		SHA256 string    `json:"sha256"`
		Digits [10]Digit `json:"digits"`
	} `json:"atlas"`
}

// Bundle 是服务端产出的完整帧包。
type Bundle struct {
	Base  []byte
	Meta  Meta
	Atlas []byte
}

// Build 渲染底帧、编码字形图集并生成索引。
func Build(r *render.Renderer, s source.Snapshot) (*Bundle, error) {
	baseJPEG, err := jpegrestart.Encode(r.BaseImage(s), jpegrestart.Options{
		Quality: Quality, RestartInterval: DRI,
	})
	if err != nil {
		return nil, fmt.Errorf("frame: 编码底帧: %w", err)
	}
	if got := MCUCols * MCURows; len(baseJPEG.MCUs) != got {
		return nil, fmt.Errorf("frame: MCU 数 %d，要 %d", len(baseJPEG.MCUs), got)
	}

	b := &Bundle{Base: baseJPEG.JPEG}
	b.Meta.V = Version
	b.Meta.Width, b.Meta.Height = Width, Height
	sum := sha256.Sum256(b.Base)
	b.Meta.Frame = hex.EncodeToString(sum[:])
	b.Meta.JPEG.DRI = DRI
	b.Meta.JPEG.MCUCols = MCUCols
	b.Meta.JPEG.MCURows = MCURows
	b.Meta.Band.Rows = [2]int{render.ClockBandRow0, render.ClockBandRow1}

	// 时钟带：从第 r0 行第 0 个 interval 到第 r1 行最后一个 interval。
	bandFirst := baseJPEG.MCUs[render.ClockBandRow0*MCUCols]
	last := baseJPEG.MCUs[render.ClockBandRow1*MCUCols-1]
	b.Meta.Band.Run = Run{
		Off: bandFirst.Offset,
		Len: last.Offset + last.Length - bandFirst.Offset,
	}

	// 每行只取时钟列范围（col 72..112，5 个 interval），给设备按行拼接用。
	for row := 0; row < CardRows; row++ {
		baseRow := (render.ClockBandRow0 + row) * MCUCols
		first := baseJPEG.MCUs[baseRow+render.ClockSlots[0].MCUCol]
		lastMCU := baseJPEG.MCUs[baseRow+render.ClockSlots[3].MCUCol+DRI-1]
		b.Meta.Band.RowRuns[row] = Run{
			Off: first.Offset,
			Len: lastMCU.Offset + lastMCU.Length - first.Offset,
		}
	}

	for i, cs := range render.ClockSlots {
		slot := Slot{X: cs.X, Y: cs.Y, W: cs.W, H: cs.H, MCUCol: cs.MCUCol}
		for row := 0; row < CardRows; row++ {
			baseRow := (render.ClockBandRow0 + row) * MCUCols
			first := baseJPEG.MCUs[baseRow+cs.MCUCol]
			lastMCU := baseJPEG.MCUs[baseRow+cs.MCUCol+DRI-1]
			slot.Runs[row] = Run{
				Off: first.Offset,
				Len: lastMCU.Offset + lastMCU.Length - first.Offset,
			}
		}
		b.Meta.Slots[i] = slot
	}

	// 字形图集：每个数字一块 128×192，按 DRI=8 编成 12 段。
	var atlas []byte
	for d := 0; d < 10; d++ {
		res, err := jpegrestart.Encode(r.CardTile(byte('0'+d)), jpegrestart.Options{
			Quality: Quality, RestartInterval: DRI, FinalRST: true,
		})
		if err != nil {
			return nil, fmt.Errorf("frame: 编码字形 %d: %w", d, err)
		}
		if len(res.MCUs) != DRI*CardRows {
			return nil, fmt.Errorf("frame: 字形 %d 的 MCU 数 %d", d, len(res.MCUs))
		}
		var runs [CardRows]Run
		for row := 0; row < CardRows; row++ {
			first := res.MCUs[row*DRI]
			lastMCU := res.MCUs[row*DRI+DRI-1]
			length := lastMCU.Offset + lastMCU.Length - first.Offset
			runs[row] = Run{Off: len(atlas), Len: length}
			atlas = append(atlas, res.JPEG[first.Offset:first.Offset+length]...)
		}
		b.Meta.Atlas.Digits[d] = Digit{D: d, Runs: runs}
	}
	b.Atlas = atlas
	b.Meta.Atlas.Size = len(atlas)
	sum = sha256.Sum256(atlas)
	b.Meta.Atlas.SHA256 = hex.EncodeToString(sum[:])
	b.Meta.Band.MaxBandBytes = maxBandBytes(&b.Meta)
	if err := validateBandSize(b.Meta.Band.MaxBandBytes); err != nil {
		return nil, err
	}
	return b, nil
}

// validateBandSize 是渲染器侧的护栏：max_band_bytes 一旦超过设备
// 单块缓冲上限，构建就失败，而不是等设备加载时拒包。
func validateBandSize(n int) error {
	if n > BandCap {
		return fmt.Errorf("frame: max_band_bytes %d 超过设备 BAND_CAP %d"+
			"（需同步 rom/main/stream.h）", n, BandCap)
	}
	return nil
}

// maxBandBytes 遍历所有合法 HH:MM，算出 12 行卡片区替换后的最大字节数。
// 数字段长度随字形变化（最宽的是 8，最窄的是 1），不能用某一个时刻估算。
func maxBandBytes(m *Meta) int {
	var static [CardRows]int
	for row := 0; row < CardRows; row++ {
		n := m.Band.RowRuns[row].Len
		for slot := 0; slot < 4; slot++ {
			n -= m.Slots[slot].Runs[row].Len
		}
		static[row] = n
	}
	worst := 0
	for h := 0; h < 24; h++ {
		for min := 0; min < 60; min++ {
			digits := [4]int{h / 10, h % 10, min / 10, min % 10}
			total := 0
			for row := 0; row < CardRows; row++ {
				total += static[row]
				for slot := 0; slot < 4; slot++ {
					total += m.Atlas.Digits[digits[slot]].Runs[row].Len
				}
			}
			if total > worst {
				worst = total
			}
		}
	}
	return worst
}

// MetaJSON 返回随帧下发的元数据。
func (b *Bundle) MetaJSON() ([]byte, error) {
	return json.Marshal(b.Meta)
}

// Splice 把指定 HH:MM 的数字段拼进底帧，返回一张完整 JPEG。
//
// 每段末尾的 RSTn 按它在底帧里的绝对 interval 序号改写：
// interval = (bandRow0+row)*(MCUCols/DRI) + MCUCol/DRI。
func (b *Bundle) Splice(hour, minute int) ([]byte, error) {
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return nil, fmt.Errorf("frame: 时间越界 %02d:%02d", hour, minute)
	}
	digits := [4]int{hour / 10, hour % 10, minute / 10, minute % 10}

	type repl struct {
		start, end int // 相对时钟带
		data       []byte
	}
	var reps []repl
	for slot := 0; slot < 4; slot++ {
		d := digits[slot]
		if d < 0 || d > 9 {
			return nil, fmt.Errorf("frame: 数字越界")
		}
		for row := 0; row < CardRows; row++ {
			br := b.Meta.Slots[slot].Runs[row]
			ar := b.Meta.Atlas.Digits[d].Runs[row]
			if ar.Off < 0 || ar.Len <= 0 || ar.Off+ar.Len > len(b.Atlas) {
				return nil, fmt.Errorf("frame: 图集越界 slot=%d row=%d", slot, row)
			}
			seg := append([]byte(nil), b.Atlas[ar.Off:ar.Off+ar.Len]...)
			if len(seg) < 2 || seg[len(seg)-2] != 0xFF ||
				seg[len(seg)-1] < 0xD0 || seg[len(seg)-1] > 0xD7 {
				return nil, fmt.Errorf("frame: 段尾不是 RSTn slot=%d row=%d", slot, row)
			}
			interval := (render.ClockBandRow0+row)*intervalsPerRow +
				b.Meta.Slots[slot].MCUCol/DRI
			seg[len(seg)-1] = 0xD0 + byte(interval&7)

			start := br.Off - b.Meta.Band.Run.Off
			end := start + br.Len
			if start < 0 || end > b.Meta.Band.Run.Len {
				return nil, fmt.Errorf("frame: 底帧段越界 slot=%d row=%d", slot, row)
			}
			reps = append(reps, repl{start: start, end: end, data: seg})
		}
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].start < reps[j].start })

	base := b.Base
	bandOff, bandLen := b.Meta.Band.Run.Off, b.Meta.Band.Run.Len
	out := make([]byte, 0, len(base)+len(b.Atlas)/4)
	out = append(out, base[:bandOff]...)
	pos := 0
	for _, r := range reps {
		if r.start < pos {
			return nil, fmt.Errorf("frame: 替换段重叠")
		}
		out = append(out, base[bandOff+pos:bandOff+r.start]...)
		out = append(out, r.data...)
		pos = r.end
	}
	out = append(out, base[bandOff+pos:bandOff+bandLen]...)
	out = append(out, base[bandOff+bandLen:]...)
	return out, nil
}

// ParseMeta 是给测试/工具用的反序列化。
func ParseMeta(b []byte) (*Meta, error) {
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
