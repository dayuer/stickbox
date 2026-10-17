// Package sim 是一台假设备：走真设备的注册、MQTT、取帧与拼接流程。
package sim

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/dayuer/stickbox/server/go/internal/frame"
)

const (
	// BandCap 必须与 frame.BandCap / rom/main/stream.h 一致。
	BandCap = 16 << 10

	packMagic  = "SBF1"
	packHeader = 16
)

type Bundle struct {
	Base  []byte
	Atlas []byte
	Meta  frame.Meta
}

// ParsePack 解析 GET /api/device/frame 的帧包并做设备侧校验：
// 三段长度、meta 版本、max_band_bytes、底帧/图集 SHA-256。
func ParsePack(data []byte) (*Bundle, error) {
	if len(data) < packHeader || string(data[:4]) != packMagic {
		return nil, errors.New("sim: 帧包 magic 不对")
	}
	bl := int(binary.BigEndian.Uint32(data[4:8]))
	al := int(binary.BigEndian.Uint32(data[8:12]))
	ml := int(binary.BigEndian.Uint32(data[12:16]))
	if bl <= 0 || al <= 0 || ml <= 0 {
		return nil, errors.New("sim: 帧包长度非法")
	}
	if packHeader+bl+al+ml != len(data) {
		return nil, fmt.Errorf("sim: 帧包长度不一致（header=%d base=%d atlas=%d meta=%d）",
			packHeader, bl, al, ml)
	}
	base := data[packHeader : packHeader+bl]
	atlas := data[packHeader+bl : packHeader+bl+al]
	metaRaw := data[packHeader+bl+al:]

	meta, err := frame.ParseMeta(metaRaw)
	if err != nil {
		return nil, fmt.Errorf("sim: meta 不是合法 JSON：%w", err)
	}
	if meta.V != frame.Version {
		return nil, fmt.Errorf("sim: 帧格式版本 %d，设备支持 %d", meta.V, frame.Version)
	}
	if meta.JPEG.DRI != frame.DRI || meta.JPEG.MCUCols != frame.MCUCols ||
		meta.JPEG.MCURows != frame.MCURows {
		return nil, fmt.Errorf("sim: JPEG 参数不匹配：DRI=%d cols=%d rows=%d",
			meta.JPEG.DRI, meta.JPEG.MCUCols, meta.JPEG.MCURows)
	}
	if meta.Band.MaxBandBytes <= 0 || meta.Band.MaxBandBytes > BandCap {
		return nil, fmt.Errorf("sim: max_band_bytes %d 超过 BAND_CAP %d，拒收整包",
			meta.Band.MaxBandBytes, BandCap)
	}
	if meta.Atlas.Size != len(atlas) {
		return nil, fmt.Errorf("sim: 图集长度 %d，meta 写的是 %d", len(atlas), meta.Atlas.Size)
	}
	bs := sha256.Sum256(base)
	if meta.Frame != hex.EncodeToString(bs[:]) {
		return nil, errors.New("sim: 底帧 SHA-256 不匹配")
	}
	as := sha256.Sum256(atlas)
	if meta.Atlas.SHA256 != hex.EncodeToString(as[:]) {
		return nil, errors.New("sim: 图集 SHA-256 不匹配")
	}
	return &Bundle{
		Base:  append([]byte(nil), base...),
		Atlas: append([]byte(nil), atlas...),
		Meta:  *meta,
	}, nil
}

// Splice 按 FRAME.md 的逐行规则把 HH:MM 拼进底帧。
//
// 以 band.row_runs[row].off 为基准（不是整带的 band.run.off）：
// 取该行时钟列的字节，把 4 张卡片区间换成图集里的数字段，再把行间
// 的静态列原样接上。结果应与 frame.Bundle.Splice 逐字节一致。
func Splice(b *Bundle, hour, minute int) ([]byte, error) {
	if b == nil || len(b.Base) == 0 || len(b.Atlas) == 0 {
		return nil, errors.New("sim: 没有帧包")
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return nil, fmt.Errorf("sim: 时间越界 %02d:%02d", hour, minute)
	}
	m := &b.Meta
	if m.Band.MaxBandBytes <= 0 || m.Band.MaxBandBytes > BandCap {
		return nil, fmt.Errorf("sim: max_band_bytes %d 超过 BAND_CAP %d", m.Band.MaxBandBytes, BandCap)
	}
	if m.JPEG.DRI <= 0 || m.JPEG.MCUCols <= 0 {
		return nil, errors.New("sim: meta 的 JPEG 参数非法")
	}
	intervalsPerRow := m.JPEG.MCUCols / m.JPEG.DRI
	digits := [4]int{hour / 10, hour % 10, minute / 10, minute % 10}

	rowRuns := m.Band.RowRuns
	if rowRuns[0].Off < 0 || rowRuns[0].Len <= 0 || rowRuns[0].Off > len(b.Base) {
		return nil, errors.New("sim: row_runs 越界")
	}

	out := make([]byte, 0, len(b.Base)+len(b.Atlas)/4)
	out = append(out, b.Base[:rowRuns[0].Off]...)
	for row := 0; row < frame.CardRows; row++ {
		rr := rowRuns[row]
		if rr.Off < 0 || rr.Len <= 0 || rr.Off+rr.Len > len(b.Base) {
			return nil, fmt.Errorf("sim: row %d 的 row_runs 越界", row)
		}
		rowBuf := make([]byte, 0, rr.Len+4096)
		pos := rr.Off
		for slot := 0; slot < 4; slot++ {
			run := m.Slots[slot].Runs[row]
			if run.Off < pos || run.Off+run.Len > rr.Off+rr.Len {
				return nil, fmt.Errorf("sim: slot %d row %d 与 row_runs 不匹配", slot, row)
			}
			rowBuf = append(rowBuf, b.Base[pos:run.Off]...)

			ar := m.Atlas.Digits[digits[slot]].Runs[row]
			if ar.Off < 0 || ar.Len < 2 || ar.Off+ar.Len > len(b.Atlas) {
				return nil, fmt.Errorf("sim: 图集越界 slot=%d row=%d", slot, row)
			}
			seg := append([]byte(nil), b.Atlas[ar.Off:ar.Off+ar.Len]...)
			if seg[len(seg)-2] != 0xFF || seg[len(seg)-1] < 0xD0 || seg[len(seg)-1] > 0xD7 {
				return nil, fmt.Errorf("sim: 数字段结尾不是 RSTn slot=%d row=%d", slot, row)
			}
			interval := (m.Band.Rows[0]+row)*intervalsPerRow + m.Slots[slot].MCUCol/m.JPEG.DRI
			seg[len(seg)-1] = 0xD0 + byte(interval&7)
			rowBuf = append(rowBuf, seg...)
			pos = run.Off + run.Len
		}
		rowBuf = append(rowBuf, b.Base[pos:rr.Off+rr.Len]...)
		out = append(out, rowBuf...)

		if row < frame.CardRows-1 {
			next := rowRuns[row+1].Off
			if next < rr.Off+rr.Len || next > len(b.Base) {
				return nil, fmt.Errorf("sim: row %d 与下一行之间的静态段越界", row)
			}
			out = append(out, b.Base[rr.Off+rr.Len:next]...)
		}
	}
	last := rowRuns[frame.CardRows-1]
	out = append(out, b.Base[last.Off+last.Len:]...)
	return out, nil
}
