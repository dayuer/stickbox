package sales

import (
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func good() Report {
	return Report{
		Date:   "2026-10-17",
		TZ:     "Asia/Shanghai",
		GMV:    &Metric{Value: 146_000, Goal: 200_000},
		Orders: &Metric{Value: 420, Goal: 400},
	}
}

func TestValidate(t *testing.T) {
	if err := good().Validate(now); err != nil {
		t.Fatalf("正常数据被拒: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Report)
	}{
		{"缺日期", func(r *Report) { r.Date = "" }},
		{"日期格式错", func(r *Report) { r.Date = "2026/10/17" }},
		{"一个指标都没有", func(r *Report) { r.GMV, r.Orders, r.CardRevenue = nil, nil, nil }},
		{"负实际值", func(r *Report) { r.GMV.Value = -1 }},
		{"负目标", func(r *Report) { r.Orders.Goal = -1 }},
		// 单位搞错（元写成分）会让环永远 100%，宁可拒掉。
		{"超目标百倍", func(r *Report) { r.GMV.Value = r.GMV.Goal*maxOverGoal + 1 }},
		{"时区过长", func(r *Report) { r.TZ = string(make([]byte, 65)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := good()
			tc.mut(&r)
			err := r.Validate(now)
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("应判非法，得到 %v", err)
			}
		})
	}
}

// 没设目标是合法的（只是不上屏），不能当成非法数据拒掉整条上报。
func TestValidateAllowsZeroGoal(t *testing.T) {
	r := good()
	r.GMV.Goal = 0
	if err := r.Validate(now); err != nil {
		t.Errorf("目标为 0 应当允许: %v", err)
	}
}

func TestNormalizeTZ(t *testing.T) {
	for in, want := range map[string]string{
		"Asia/Shanghai":       "Asia/Shanghai",
		"posix/Asia/Shanghai": "Asia/Shanghai",
		"right/Asia/Shanghai": "Asia/Shanghai",
		"Mars/Olympus":        "",
		"":                    "",
	} {
		if got := NormalizeTZ(in); got != want {
			t.Errorf("NormalizeTZ(%q) = %q，要 %q", in, got, want)
		}
	}
}

func TestKindLabelsAreChinese(t *testing.T) {
	// 结论句是 label 直接拼后缀，label 以拉丁字母结尾会读成
	// 「今日 GMV进度偏慢」，所以这里不许出现 ASCII 字母。
	for _, k := range []Kind{KindGMV, KindOrders, KindCardRevenue} {
		for _, r := range k.Label() {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				t.Errorf("%s 的名字 %q 含拉丁字母，拼结论句会读不通", k, k.Label())
				break
			}
		}
	}
}
