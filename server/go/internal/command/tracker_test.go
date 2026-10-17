package command

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

type sent struct {
	topic   string
	payload []byte
	retain  bool
}

type fakePub struct {
	mu   sync.Mutex
	msgs []sent
	err  error
}

func (f *fakePub) PublishRaw(topic string, payload []byte, retain bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	cp := append([]byte(nil), payload...)
	f.msgs = append(f.msgs, sent{topic, cp, retain})
	return nil
}

func (f *fakePub) all() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.msgs...)
}

type fakePresence struct {
	mu     sync.Mutex
	online map[string]bool
}

func (p *fakePresence) IsOnline(dev string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.online[dev]
}

func (p *fakePresence) set(dev string, up bool) {
	p.mu.Lock()
	p.online[dev] = up
	p.mu.Unlock()
}

func cidOf(t *testing.T, payload []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("payload 不是 JSON：%s", payload)
	}
	s, _ := m["cid"].(string)
	return s
}

func newTracker(t *testing.T) (*Tracker, *fakePub, *fakePresence, func() time.Time, *time.Time) {
	t.Helper()
	pub := &fakePub{}
	pres := &fakePresence{online: map[string]bool{}}
	tr := New(pub, pres)
	clock := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	tr.now = func() time.Time { return clock }
	return tr, pub, pres, tr.now, &clock
}

// 下行必须带上 cid，否则设备无从回执。
func TestSendInjectsCID(t *testing.T) {
	tr, pub, _, _, _ := newTracker(t)
	cid, err := tr.Send("f412fa87c9e0", "printer/f412fa87c9e0/usage", []byte(`{"now":1}`), false)
	if err != nil {
		t.Fatal(err)
	}
	msgs := pub.all()
	if len(msgs) != 1 {
		t.Fatalf("发了 %d 条", len(msgs))
	}
	if got := cidOf(t, msgs[0].payload); got != cid {
		t.Errorf("payload 里的 cid=%q，Send 返回 %q", got, cid)
	}
	// 原有字段不能丢。
	var m map[string]any
	json.Unmarshal(msgs[0].payload, &m)
	if m["now"] == nil {
		t.Error("注入 cid 把原有字段弄丢了")
	}
	if tr.Pending("f412fa87c9e0") != 1 {
		t.Error("发出去之后应当记成待确认")
	}
}

// retain 要透传：pair 靠它让重连的设备拿到当前状态。
func TestSendPassesRetain(t *testing.T) {
	tr, pub, _, _, _ := newTracker(t)
	tr.Send("d", "printer/d/pair", []byte(`{"paired":true}`), true)
	if !pub.all()[0].retain {
		t.Error("retain 没透传")
	}
}

// 不是 JSON 对象就塞不进 cid，必须直接报错而不是悄悄发一条无法确认的指令。
func TestSendRejectsNonObject(t *testing.T) {
	tr, _, _, _, _ := newTracker(t)
	for _, bad := range []string{`[1,2]`, `"x"`, `notjson`} {
		if _, err := tr.Send("d", "printer/d/usage", []byte(bad), false); err == nil {
			t.Errorf("payload=%s 应当报错", bad)
		}
	}
}

// 收到 ACK 就结案，不再重投。
func TestAckClearsPending(t *testing.T) {
	tr, pub, pres, _, clock := newTracker(t)
	pres.set("d", true)
	cid, _ := tr.Send("d", "printer/d/usage", []byte(`{"now":1}`), false)
	tr.Ack("d", cid, true, "")

	if tr.Pending("d") != 0 {
		t.Error("ACK 之后不该还有待确认")
	}
	*clock = clock.Add(10 * time.Minute)
	tr.sweep()
	if len(pub.all()) != 1 {
		t.Errorf("已确认的指令被重投了，共 %d 条", len(pub.all()))
	}
	if o, ok := tr.LastOutcome("d"); !ok || !o.OK {
		t.Errorf("结局 = %+v ok=%v", o, ok)
	}
}

// 没收到 ACK 就按退避重投。
func TestRetriesUntilAcked(t *testing.T) {
	tr, pub, pres, _, clock := newTracker(t)
	pres.set("d", true)
	tr.Send("d", "printer/d/usage", []byte(`{"now":1}`), false)

	// 时间没到不重投。
	tr.sweep()
	if len(pub.all()) != 1 {
		t.Fatalf("时间没到就重投了，共 %d 条", len(pub.all()))
	}
	*clock = clock.Add(retryBase + time.Second)
	tr.sweep()
	if len(pub.all()) != 2 {
		t.Fatalf("到期没重投，共 %d 条", len(pub.all()))
	}
	// 重投的必须是同一个 cid——换了 cid 设备的幂等判断就失效了。
	if cidOf(t, pub.all()[0].payload) != cidOf(t, pub.all()[1].payload) {
		t.Error("重投换了 cid")
	}
}

// 投满上限仍无回执就放弃，并把结局记成失败——两端状态不一致必须看得见。
func TestGivesUpAfterMaxTries(t *testing.T) {
	tr, pub, pres, _, clock := newTracker(t)
	pres.set("d", true)
	tr.Send("d", "printer/d/usage", []byte(`{"now":1}`), false)

	for i := 0; i < maxTries+3; i++ {
		*clock = clock.Add(retryMax + time.Second)
		tr.sweep()
	}
	if n := len(pub.all()); n != maxTries {
		t.Errorf("共投了 %d 次，期望 %d", n, maxTries)
	}
	if tr.Pending("d") != 0 {
		t.Error("放弃之后不该还挂着")
	}
	o, ok := tr.LastOutcome("d")
	if !ok || o.OK || o.Err != "no_ack" {
		t.Errorf("结局 = %+v ok=%v，期望失败且 err=no_ack", o, ok)
	}
}

// 设备回 ok=0 是永久失败：它收到了但刻意不应用，重投还会是同样结果。
func TestNackIsPermanent(t *testing.T) {
	tr, pub, pres, _, clock := newTracker(t)
	pres.set("d", true)
	cid, _ := tr.Send("d", "printer/d/pair", []byte(`{"paired":true}`), true)
	tr.Ack("d", cid, false, "stale_key")

	*clock = clock.Add(10 * time.Minute)
	tr.sweep()
	if len(pub.all()) != 1 {
		t.Errorf("ok=0 之后还在重投，共 %d 条", len(pub.all()))
	}
	if o, _ := tr.LastOutcome("d"); o.OK || o.Err != "stale_key" {
		t.Errorf("结局 = %+v", o)
	}
}

// 设备离线时不重投：发出去也没人收，白烧重试预算。上线后才继续。
func TestNoRetryWhileOffline(t *testing.T) {
	tr, pub, pres, _, clock := newTracker(t)
	pres.set("d", false)
	tr.Send("d", "printer/d/usage", []byte(`{"now":1}`), false)

	for i := 0; i < 10; i++ {
		*clock = clock.Add(retryMax + time.Second)
		tr.sweep()
	}
	if len(pub.all()) != 1 {
		t.Fatalf("离线时重投了，共 %d 条", len(pub.all()))
	}
	if tr.Pending("d") != 1 {
		t.Error("离线不该把它判死")
	}
	pres.set("d", true)
	*clock = clock.Add(retryMax + time.Second)
	tr.sweep()
	if len(pub.all()) != 2 {
		t.Error("上线后没有补投")
	}
}

// 设备对已结案的指令补 ACK（重投撞上在途回执），不能 panic 也不该算错。
func TestAckUnknownCIDIsHarmless(t *testing.T) {
	tr, _, _, _, _ := newTracker(t)
	tr.Ack("d", "nosuchcid", true, "")
	if tr.Pending("d") != 0 {
		t.Error("不该凭空产生待确认项")
	}
}

// 发布失败时不能留下一条永远重投不出去的记录。
func TestSendFailureDoesNotTrack(t *testing.T) {
	tr, pub, _, _, _ := newTracker(t)
	pub.err = errSentinel
	if _, err := tr.Send("d", "printer/d/usage", []byte(`{"now":1}`), false); err == nil {
		t.Fatal("发布失败应当返回错误")
	}
	if tr.Pending("d") != 0 {
		t.Error("发布失败却记成了待确认")
	}
}

var errSentinel = errSentinelType{}

type errSentinelType struct{}

func (errSentinelType) Error() string { return "boom" }
