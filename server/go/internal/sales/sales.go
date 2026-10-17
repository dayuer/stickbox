// Package sales 是「超代当日经营数据」这一路数据的上报格式。
//
// 和 usage 并列：usage 是 Claude 额度（消耗型，用满是告警），sales 是当日
// 经营指标（达成型，达成是业绩）。两者都只定义自己的线格式与校验，换算成
// 看板窗口的事交给 internal/source。
package sales

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrInvalid = errors.New("sales: 数据非法")

// Kind 是一个经营指标的种类。版面只有两个环，上报可以带三个指标，
// 由数据源决定哪两个上屏。
type Kind string

const (
	KindGMV         Kind = "gmv"          // 当日成交额
	KindOrders      Kind = "orders"       // 当日订单数
	KindCardRevenue Kind = "card_revenue" // 权益卡收入
)

// Metric 是一个指标的当日实际值与目标值。
//
// Goal <= 0 表示这个指标没有设目标：环画不出达成率，这一路就不上屏。
type Metric struct {
	Value float64 `json:"value"`
	Goal  float64 `json:"goal"`
}

// Report 是一次当日经营数据上报。
type Report struct {
	// Date 是这份数据所属的本地日期（YYYY-MM-DD）。
	// 跨零点后旧数据不能继续当成「今日」显示，所以必须带日期而不是只带时间戳。
	Date string `json:"date"`
	// TZ 是上报方的 IANA 时区；当日窗口的零点按它算，不能按服务器的 UTC。
	TZ string `json:"tz,omitempty"`

	GMV         *Metric `json:"gmv,omitempty"`
	Orders      *Metric `json:"orders,omitempty"`
	CardRevenue *Metric `json:"card_revenue,omitempty"`
}

// Metric 按种类取指标，没有这一项时返回 nil。
func (r Report) Metric(k Kind) *Metric {
	switch k {
	case KindGMV:
		return r.GMV
	case KindOrders:
		return r.Orders
	case KindCardRevenue:
		return r.CardRevenue
	}
	return nil
}

// Label 是指标在环内显示的名字。
//
// 一律用中文：结论句是 label 直接拼后缀（"今日成交额进度偏慢"），
// label 以拉丁字母结尾会读成「今日 GMV进度偏慢」。
func (k Kind) Label() string {
	switch k {
	case KindGMV:
		return "今日成交额"
	case KindOrders:
		return "今日订单"
	case KindCardRevenue:
		return "权益卡收入"
	}
	return "今日指标"
}

// 实际值给一点超目标的余量，但不接受离谱的数（一般是单位搞错了）。
const maxOverGoal = 100

// Validate 拒绝明显不对的数据。
func (r Report) Validate(now time.Time) error {
	if _, err := time.Parse("2006-01-02", r.Date); err != nil {
		return fmt.Errorf("%w: date 要是 YYYY-MM-DD", ErrInvalid)
	}
	if len(r.TZ) > 64 {
		return fmt.Errorf("%w: tz 过长", ErrInvalid)
	}
	if r.GMV == nil && r.Orders == nil && r.CardRevenue == nil {
		return fmt.Errorf("%w: 至少要有一个指标", ErrInvalid)
	}
	for _, k := range []Kind{KindGMV, KindOrders, KindCardRevenue} {
		m := r.Metric(k)
		if m == nil {
			continue
		}
		if m.Value < 0 || m.Goal < 0 {
			return fmt.Errorf("%w: %s 不能为负", ErrInvalid, k)
		}
		if m.Goal > 0 && m.Value > m.Goal*maxOverGoal {
			return fmt.Errorf("%w: %s 实际值超目标 %d 倍，疑似单位错", ErrInvalid, k, maxOverGoal)
		}
	}
	return nil
}

// NormalizeTZ 清掉无法识别的时区名，让它回退到服务端默认值。
// 和 usage 一样：时区只影响排版，不该让整条上报被拒。
func NormalizeTZ(tz string) string {
	if tz == "" {
		return ""
	}
	clean := strings.TrimPrefix(strings.TrimPrefix(tz, "posix/"), "right/")
	if _, err := time.LoadLocation(clean); err != nil {
		return ""
	}
	return clean
}
