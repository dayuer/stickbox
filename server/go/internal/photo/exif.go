package photo

import "encoding/binary"

// jpegOrientation 从 JPEG 的 APP1/Exif 里读 Orientation（tag 0x0112）。
// 只读这一个字段；其余元数据在重新编码时自然全部丢掉。
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xFF {
			i++
			continue
		}
		marker := data[i+1]
		// 无长度的标记。
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 { // SOS/EOI，后面是图像数据
			break
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(data) {
			break
		}
		payload := data[i+4 : i+2+segLen]
		if marker == 0xE1 && len(payload) >= 6 && string(payload[:6]) == "Exif\x00\x00" {
			return parseTIFFOrientation(payload[6:])
		}
		i += 2 + segLen
	}
	return 1
}

func parseTIFFOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(t[2:4]) != 42 {
		return 1
	}
	off := int(order.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := int(order.Uint16(t[off : off+2]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		tag := order.Uint16(t[e : e+2])
		if tag != 0x0112 {
			continue
		}
		typ := order.Uint16(t[e+2 : e+4])
		count := order.Uint32(t[e+4 : e+8])
		if typ != 3 || count != 1 {
			return 1
		}
		v := int(order.Uint16(t[e+8 : e+10]))
		if v >= 1 && v <= 8 {
			return v
		}
		return 1
	}
	return 1
}
