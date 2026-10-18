package theme

import "testing"

func quotaGeom() Geometry {
	return Geometry{
		BandRow0: 4, BandRow1: 16,
		Slots: [NumSlots]ClockSlot{
			{1152, 64, 128, 192, 72},
			{1280, 64, 128, 192, 80},
			{1536, 64, 128, 192, 96},
			{1664, 64, 128, 192, 104},
		},
	}
}

// 现有版面的几何必须过校验——它是 D 方案的基准。
func TestGeometryValidateAcceptsQuota(t *testing.T) {
	if err := quotaGeom().Validate(); err != nil {
		t.Fatalf("现有几何被拒: %v", err)
	}
}

// 这些错不会在服务端炸，而是在电视上显示成错位的数字，所以必须在出包前拦住。
func TestGeometryValidateRejects(t *testing.T) {
	cases := map[string]func(*Geometry){
		"带行数不对":        func(g *Geometry) { g.BandRow1 = 15 },
		"带超出画面":        func(g *Geometry) { g.BandRow0, g.BandRow1 = 60, 72 },
		"槽没落在 DRI 边界":  func(g *Geometry) { g.Slots[1].MCUCol, g.Slots[1].X = 81, 1296 },
		"槽列没递增":        func(g *Geometry) { g.Slots[2].MCUCol, g.Slots[2].X = 72, 1152 },
		"x 与 MCU 列对不上": func(g *Geometry) { g.Slots[0].X = 1160 },
		"槽没占满带":        func(g *Geometry) { g.Slots[0].H = 160 },
		"槽 y 不在带起点":    func(g *Geometry) { g.Slots[3].Y = 80 },
		"宽不是整数个区间":     func(g *Geometry) { g.Slots[0].W = 100 },
		"槽超出右边缘":       func(g *Geometry) { g.Slots[3].MCUCol, g.Slots[3].X = 118, 1888 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			g := quotaGeom()
			mut(&g)
			if err := g.Validate(); err == nil {
				t.Error("应当被拒，但过了")
			}
		})
	}
}
