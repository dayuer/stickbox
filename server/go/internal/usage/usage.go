// Package usage 处理 Claude 订阅用量：agent 上报 → 落库 → 推给用户名下的设备。
//
// 数据从哪来：用户电脑上的 stickbox-agent 用用户**自己的** Claude 登录执行
// `claude -p /usage`，把结果带 agent 令牌传上来。服务端从不接触 Anthropic 的凭据——
// 订阅用量没有开放给第三方的授权接口，托管别人的 Claude 登录既违反条款也是隐患。
//
// 设备没有 SNTP，所以推给设备的每一份都现场盖上服务端时间 now；
// 设备拿它和自己的开机时长对表，倒计时才走得对。也因此 MQTT 不用 retain——
// retain 的那份 now 是旧的，设备重连时由 Resend 现场补发一份新的。
package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Window 是一个限额窗口，字段名沿用 Claude Code statusline 的 rate_limits。
type Window struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"` // Unix 秒
}

// Report 是 agent 上报的内容。
type Report struct {
	FiveHour *Window `json:"five_hour,omitempty"`
	SevenDay *Window `json:"seven_day,omitempty"`
	// Limits 是 /usage 原始的 limits 行，原样存给 App 看（按模型的周用量等）。
	// 设备用不上，不下发。
	Limits json.RawMessage `json:"limits,omitempty"`
}

var ErrInvalid = errors.New("usage: 数据非法")

// 原始 limits 最多存这么大。正常只有几行，几百字节。
const maxLimitsBytes = 8 << 10

// Validate 拒绝明显不对的数据。百分比给一点超 100 的余量（服务端偶尔报 100+），
// 重置时间不能离谱地远——7 天窗口最远也就 7 天后。
func (r Report) Validate(now time.Time) error {
	if r.FiveHour == nil && r.SevenDay == nil {
		return fmt.Errorf("%w: 至少要有一个窗口", ErrInvalid)
	}
	for _, w := range []*Window{r.FiveHour, r.SevenDay} {
		if w == nil {
			continue
		}
		if w.UsedPercentage < 0 || w.UsedPercentage > 1000 {
			return fmt.Errorf("%w: used_percentage 越界", ErrInvalid)
		}
		if w.ResetsAt <= 0 || w.ResetsAt > now.Add(30*24*time.Hour).Unix() {
			return fmt.Errorf("%w: resets_at 越界", ErrInvalid)
		}
	}
	if len(r.Limits) > maxLimitsBytes {
		return fmt.Errorf("%w: limits 过大", ErrInvalid)
	}
	return nil
}

// DevicePayload 是推给设备的那份。字段见 docs/API-cloud-print.md 3.10。
//
// ts 是服务端收到这份数据的时间：设备据此显示「更新 X 分前」。
// 不能用设备收到的时刻——重连补发会让一份很旧的数据看起来像刚到。
func DevicePayload(r Report, receivedAt, now time.Time) []byte {
	b, _ := json.Marshal(struct {
		Now      int64   `json:"now"`
		TS       int64   `json:"ts"`
		FiveHour *Window `json:"five_hour,omitempty"`
		SevenDay *Window `json:"seven_day,omitempty"`
	}{now.Unix(), receivedAt.Unix(), r.FiveHour, r.SevenDay})
	return b
}

// Store 是 usage 对 store 的窄依赖。
type Store interface {
	PutUsage(userID string, body []byte, received int64) error
	GetUsage(userID string) (body []byte, received int64, ok bool, err error)
	DevicesOfUser(userID string) ([]string, error)
	OwnerOfDevice(dev string) (string, bool, error)
}

// Publisher 是 usage 对 MQTT 的窄依赖。
type Publisher interface {
	PublishUsage(dev string, payload []byte) error
}

type Service struct {
	st  Store
	pub Publisher
	now func() time.Time
}

func NewService(st Store, pub Publisher, now func() time.Time) *Service {
	return &Service{st: st, pub: pub, now: now}
}

// Ingest 落库并推给该用户名下的全部设备，返回推送成功的台数。
//
// 推送失败不算上报失败：数据已经落库，设备下次重连会由 Resend 补上。
func (s *Service) Ingest(userID string, r Report) (int, error) {
	now := s.now()
	if err := r.Validate(now); err != nil {
		return 0, err
	}
	body, err := json.Marshal(r)
	if err != nil {
		return 0, err
	}
	if err := s.st.PutUsage(userID, body, now.Unix()); err != nil {
		return 0, err
	}
	devs, err := s.st.DevicesOfUser(userID)
	if err != nil {
		return 0, err
	}
	payload := DevicePayload(r, now, now)
	sent := 0
	for _, d := range devs {
		if err := s.pub.PublishUsage(d, payload); err != nil {
			slog.Warn("推送用量失败", "dev", d, "err", err)
			continue
		}
		sent++
	}
	return sent, nil
}

// Latest 返回该用户最近一次上报和收到的时间。
func (s *Service) Latest(userID string) (Report, time.Time, bool, error) {
	body, ts, ok, err := s.st.GetUsage(userID)
	if err != nil || !ok {
		return Report{}, time.Time{}, false, err
	}
	var r Report
	if err := json.Unmarshal(body, &r); err != nil {
		return Report{}, time.Time{}, false, err
	}
	return r, time.Unix(ts, 0), true, nil
}

// Resend 把设备主人的最新用量补发给这台设备。设备订阅 usage 主题时调用。
func (s *Service) Resend(dev string) {
	owner, bound, err := s.st.OwnerOfDevice(dev)
	if err != nil || !bound {
		return
	}
	r, received, ok, err := s.Latest(owner)
	if err != nil {
		slog.Warn("读用量失败", "dev", dev, "err", err)
		return
	}
	if !ok {
		return // 还没有 agent 上报过
	}
	if err := s.pub.PublishUsage(dev, DevicePayload(r, received, s.now())); err != nil {
		slog.Warn("补发用量失败", "dev", dev, "err", err)
	}
}
