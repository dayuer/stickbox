package sales

import (
	"errors"
	"testing"
	"time"
)

type memStore struct {
	body     []byte
	received int64
	ok       bool
}

func (m *memStore) PutSales(_ string, body []byte, received int64) error {
	m.body, m.received, m.ok = body, received, true
	return nil
}

func (m *memStore) GetSales(string) ([]byte, int64, bool, error) {
	return m.body, m.received, m.ok, nil
}

func svc(st Store) *Service {
	return NewService(st, func() time.Time { return now })
}

func TestIngestThenLatest(t *testing.T) {
	st := &memStore{}
	s := svc(st)
	if _, _, ok, err := s.Latest("u1"); ok || err != nil {
		t.Fatalf("还没上报时 ok=%v err=%v", ok, err)
	}
	if err := s.Ingest("u1", good()); err != nil {
		t.Fatal(err)
	}
	got, received, ok, err := s.Latest("u1")
	if err != nil || !ok {
		t.Fatalf("读回失败 ok=%v err=%v", ok, err)
	}
	if !received.Equal(now) {
		t.Errorf("到达时刻 = %v，要 %v", received, now)
	}
	if got.Date != "2026-10-17" || got.TZ != "Asia/Shanghai" {
		t.Errorf("元数据不对: %+v", got)
	}
	if got.GMV == nil || got.GMV.Value != 146_000 || got.Orders == nil || got.Orders.Goal != 400 {
		t.Errorf("指标没存全: %+v", got)
	}
}

// 第二次上报覆盖第一次：只留最新一份。
func TestIngestOverwrites(t *testing.T) {
	st := &memStore{}
	s := svc(st)
	if err := s.Ingest("u1", good()); err != nil {
		t.Fatal(err)
	}
	r2 := good()
	r2.GMV.Value = 180_000
	if err := s.Ingest("u1", r2); err != nil {
		t.Fatal(err)
	}
	got, _, _, _ := s.Latest("u1")
	if got.GMV.Value != 180_000 {
		t.Errorf("成交额 = %v，要 180000", got.GMV.Value)
	}
}

// 非法数据不落库：上一份好数据要留着。
func TestIngestRejectsInvalidWithoutClobbering(t *testing.T) {
	st := &memStore{}
	s := svc(st)
	if err := s.Ingest("u1", good()); err != nil {
		t.Fatal(err)
	}
	bad := good()
	bad.Date = "昨天"
	if err := s.Ingest("u1", bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v，要 ErrInvalid", err)
	}
	got, _, _, _ := s.Latest("u1")
	if got.Date != "2026-10-17" {
		t.Errorf("非法上报把好数据冲掉了: %+v", got)
	}
}

// 无法识别的时区被清掉，但整条上报要照常落库——时区只影响排版。
func TestIngestNormalizesTZ(t *testing.T) {
	st := &memStore{}
	s := svc(st)
	r := good()
	r.TZ = "posix/Asia/Shanghai"
	if err := s.Ingest("u1", r); err != nil {
		t.Fatal(err)
	}
	if got, _, _, _ := s.Latest("u1"); got.TZ != "Asia/Shanghai" {
		t.Errorf("时区 = %q，要 Asia/Shanghai", got.TZ)
	}

	r.TZ = "Mars/Olympus"
	if err := s.Ingest("u1", r); err != nil {
		t.Fatalf("无法识别的时区不该让上报被拒: %v", err)
	}
	if got, _, _, _ := s.Latest("u1"); got.TZ != "" {
		t.Errorf("时区 = %q，要空（回退默认）", got.TZ)
	}
}
