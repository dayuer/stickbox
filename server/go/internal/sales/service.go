package sales

import (
	"encoding/json"
	"log/slog"
	"time"
)

// Store 是 sales 对 store 的窄依赖。
type Store interface {
	PutSales(userID string, body []byte, received int64) error
	GetSales(userID string) (body []byte, received int64, ok bool, err error)
}

// Service 收 agent 上报的当日经营数据。
//
// 和 usage.Service 的区别：这里不往 MQTT 推。设备不认识经营指标，它只取
// 服务端渲好的帧；上报后由 HTTP 端点标脏、帧服务防抖重渲。少一条推送链路
// 就少一处两端状态不一致的来源。
type Service struct {
	st  Store
	now func() time.Time
}

func NewService(st Store, now func() time.Time) *Service {
	return &Service{st: st, now: now}
}

// Ingest 校验并落库，覆盖该用户上一份数据。
func (s *Service) Ingest(userID string, r Report) error {
	now := s.now()
	// 时区只影响排版，不合法不能让整条上报被拒（agent 的 tzdata 比服务端新，
	// 或从 /etc/localtime 截出 "posix/Asia/Shanghai"）。清掉后回退默认时区。
	if tz := NormalizeTZ(r.TZ); tz != r.TZ {
		slog.Warn("忽略无法识别的时区，回退默认", "user", userID, "tz", r.TZ)
		r.TZ = tz
	}
	if err := r.Validate(now); err != nil {
		return err
	}
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.st.PutSales(userID, body, now.Unix())
}

// Latest 返回该用户最近一次上报和收到的时间，供 source.Sales 取用。
func (s *Service) Latest(userID string) (Report, time.Time, bool, error) {
	body, ts, ok, err := s.st.GetSales(userID)
	if err != nil || !ok {
		return Report{}, time.Time{}, false, err
	}
	var r Report
	if err := json.Unmarshal(body, &r); err != nil {
		return Report{}, time.Time{}, false, err
	}
	return r, time.Unix(ts, 0), true, nil
}
