package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	pct := func(v float64) *Window { return &Window{UsedPercentage: v, ResetsAt: now.Add(time.Hour).Unix()} }

	cases := []struct {
		name string
		r    Report
		ok   bool
	}{
		{"两个窗口都没有", Report{}, false},
		{"只有 five_hour", Report{FiveHour: pct(12.5)}, true},
		{"只有 seven_day", Report{SevenDay: pct(0)}, true},
		{"百分比为负", Report{FiveHour: pct(-0.1)}, false},
		{"百分比刚好 0", Report{FiveHour: pct(0)}, true},
		{"百分比刚好 1000", Report{FiveHour: pct(1000)}, true},
		{"百分比超过 1000", Report{FiveHour: pct(1000.1)}, false},
		{"resets_at 为 0", Report{FiveHour: &Window{UsedPercentage: 1, ResetsAt: 0}}, false},
		{"resets_at 为负", Report{FiveHour: &Window{UsedPercentage: 1, ResetsAt: -1}}, false},
		{"resets_at 超过 30 天", Report{FiveHour: &Window{
			UsedPercentage: 1, ResetsAt: now.Add(30*24*time.Hour + time.Second).Unix()}}, false},
		{"limits 超过 8KB", Report{FiveHour: pct(1),
			Limits: json.RawMessage(bytes.Repeat([]byte("x"), maxLimitsBytes+1))}, false},
		{"limits 刚好 8KB", Report{FiveHour: pct(1),
			Limits: json.RawMessage(bytes.Repeat([]byte("x"), maxLimitsBytes))}, true},
		{"时区非法不影响校验", Report{FiveHour: pct(1), TZ: "posix/Asia/Shanghai"}, true},
		{"时区超长", Report{FiveHour: pct(1), TZ: string(bytes.Repeat([]byte("x"), 65))}, false},
	}
	for _, c := range cases {
		err := c.r.Validate(now)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v，期望 ok=%v", c.name, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: 错误没有包装 ErrInvalid：%v", c.name, err)
		}
	}
}

type fakeStore struct {
	devices []string
	devErr  error

	putUser string
	putBody []byte
	putAt   int64

	usageBody []byte
	usageAt   int64
	usageOK   bool
	usageErr  error

	owner    string
	bound    bool
	ownerErr error
}

func (f *fakeStore) PutUsage(userID string, body []byte, received int64) error {
	f.putUser, f.putBody, f.putAt = userID, body, received
	return nil
}

func (f *fakeStore) GetUsage(userID string) ([]byte, int64, bool, error) {
	return f.usageBody, f.usageAt, f.usageOK, f.usageErr
}

func (f *fakeStore) DevicesOfUser(userID string) ([]string, error) {
	return f.devices, f.devErr
}

func (f *fakeStore) OwnerOfDevice(dev string) (string, bool, error) {
	return f.owner, f.bound, f.ownerErr
}

type fakePub struct {
	sent map[string][]byte
	fail map[string]error
}

func (p *fakePub) PublishUsage(dev string, payload []byte) error {
	if err := p.fail[dev]; err != nil {
		return err
	}
	if p.sent == nil {
		p.sent = map[string][]byte{}
	}
	p.sent[dev] = payload
	return nil
}

func TestIngestPublishesToAllDevices(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	st := &fakeStore{devices: []string{"d1", "d2"}}
	pub := &fakePub{}
	s := NewService(st, pub, func() time.Time { return now })

	r := Report{FiveHour: &Window{UsedPercentage: 42, ResetsAt: now.Add(time.Hour).Unix()}}
	n, err := s.Ingest("u1", r)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("推送成功 %d 台，期望 2", n)
	}
	if st.putUser != "u1" || st.putAt != now.Unix() {
		t.Errorf("落库 user=%q at=%d", st.putUser, st.putAt)
	}
	for _, dev := range []string{"d1", "d2"} {
		var p struct {
			Now      int64   `json:"now"`
			TS       int64   `json:"ts"`
			FiveHour *Window `json:"five_hour"`
		}
		if err := json.Unmarshal(pub.sent[dev], &p); err != nil {
			t.Fatalf("%s 的 payload 不是 JSON：%v", dev, err)
		}
		if p.Now != now.Unix() || p.TS != now.Unix() {
			t.Errorf("%s: now=%d ts=%d，期望都是 %d", dev, p.Now, p.TS, now.Unix())
		}
		if p.FiveHour == nil || p.FiveHour.UsedPercentage != 42 {
			t.Errorf("%s: five_hour = %+v", dev, p.FiveHour)
		}
	}
}

// 时区不合法只丢时区，用量照常收下。
func TestIngestDropsUnknownTZ(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	st := &fakeStore{devices: []string{"d1"}}
	pub := &fakePub{}
	s := NewService(st, pub, func() time.Time { return now })

	mk := func(tz string) Report {
		return Report{
			FiveHour: &Window{UsedPercentage: 1, ResetsAt: now.Add(time.Hour).Unix()},
			TZ:       tz,
		}
	}

	// posix/ 前缀去掉后是合法时区。
	if _, err := s.Ingest("u1", mk("posix/Asia/Shanghai")); err != nil {
		t.Fatalf("posix 前缀不该让上报失败：%v", err)
	}
	var stored Report
	json.Unmarshal(st.putBody, &stored)
	if stored.TZ != "Asia/Shanghai" {
		t.Errorf("存下的 tz = %q，期望 Asia/Shanghai", stored.TZ)
	}

	// 完全不认识的时区清空，用量仍然收下。
	if _, err := s.Ingest("u1", mk("Not/AZone")); err != nil {
		t.Fatalf("未知时区不该让上报失败：%v", err)
	}
	var stored2 Report
	json.Unmarshal(st.putBody, &stored2)
	if stored2.TZ != "" {
		t.Errorf("未知时区没被清空：%q", stored2.TZ)
	}
}

// 推送失败不算上报失败：数据已经落库，设备重连时会补发。
func TestIngestSucceedsWhenSomePublishFail(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	st := &fakeStore{devices: []string{"d1", "d2"}}
	pub := &fakePub{fail: map[string]error{"d2": errors.New("offline")}}
	s := NewService(st, pub, func() time.Time { return now })

	n, err := s.Ingest("u1", Report{
		FiveHour: &Window{UsedPercentage: 1, ResetsAt: now.Add(time.Hour).Unix()},
	})
	if err != nil {
		t.Fatalf("单台推送失败不该让上报失败：%v", err)
	}
	if n != 1 {
		t.Errorf("成功台数 = %d，期望 1", n)
	}
}

func TestIngestRejectsInvalidBeforeStoring(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	st := &fakeStore{devices: []string{"d1"}}
	pub := &fakePub{}
	s := NewService(st, pub, func() time.Time { return now })

	if _, err := s.Ingest("u1", Report{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v，期望 ErrInvalid", err)
	}
	if st.putUser != "" {
		t.Error("非法数据也落库了")
	}
	if len(pub.sent) != 0 {
		t.Error("非法数据也推送了")
	}
}

// 补发时 now 是现场时间，ts 仍是服务端收到数据的时间。
// 设备靠 ts 算「更新 X 分前」——用补发时刻会让旧数据看起来像刚到。
func TestResendStampsNowAtResendTime(t *testing.T) {
	received := time.Unix(1_000_000, 0)
	now := received.Add(5 * time.Minute)
	body, _ := json.Marshal(Report{
		FiveHour: &Window{UsedPercentage: 7, ResetsAt: received.Add(time.Hour).Unix()},
	})
	st := &fakeStore{owner: "u1", bound: true, usageBody: body,
		usageAt: received.Unix(), usageOK: true}
	pub := &fakePub{}
	s := NewService(st, pub, func() time.Time { return now })

	s.Resend("d1")

	var p struct {
		Now      int64   `json:"now"`
		TS       int64   `json:"ts"`
		FiveHour *Window `json:"five_hour"`
	}
	if err := json.Unmarshal(pub.sent["d1"], &p); err != nil {
		t.Fatalf("payload 不是 JSON：%v", err)
	}
	if p.Now != now.Unix() {
		t.Errorf("now = %d，期望补发时刻 %d", p.Now, now.Unix())
	}
	if p.TS != received.Unix() {
		t.Errorf("ts = %d，期望收到时刻 %d", p.TS, received.Unix())
	}
	if p.FiveHour == nil || p.FiveHour.UsedPercentage != 7 {
		t.Errorf("five_hour = %+v", p.FiveHour)
	}
}

func TestResendSkipsUnboundDevice(t *testing.T) {
	st := &fakeStore{bound: false}
	pub := &fakePub{}
	s := NewService(st, pub, time.Now)
	s.Resend("d1")
	if len(pub.sent) != 0 {
		t.Error("未绑定的设备不该收到补发")
	}
}

func TestResendSkipsWhenNoReport(t *testing.T) {
	st := &fakeStore{owner: "u1", bound: true, usageOK: false}
	pub := &fakePub{}
	s := NewService(st, pub, time.Now)
	s.Resend("d1")
	if len(pub.sent) != 0 {
		t.Error("还没有上报过就不该发")
	}
}
