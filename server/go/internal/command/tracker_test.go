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

// —— 审计第一轮发现的问题 ——

// 跨设备伪造 ACK：xid 可预测，恶意设备能猜到别人的 cid 替它结案。
func TestAckFromWrongDeviceIsIgnored(t *testing.T) {
	tr, pub, pres, _, clock := newTracker(t)
	pres.set("victim", true)
	cid, _ := tr.Send("victim", "printer/victim/pair", []byte(`{"paired":false}`), true)

	tr.Ack("attacker", cid, true, "")

	if tr.Pending("victim") != 1 {
		t.Fatal("别的设备把受害者的指令结案了")
	}
	if _, ok := tr.LastOutcome("victim"); ok {
		t.Error("不该产生结局记录")
	}
	// 真正的设备仍然能正常确认。
	tr.Ack("victim", cid, true, "")
	if tr.Pending("victim") != 0 {
		t.Error("本人的 ACK 没生效")
	}
	*clock = clock.Add(time.Hour)
	tr.sweep()
	if len(pub.all()) != 1 {
		t.Errorf("确认后还在重投，共 %d 条", len(pub.all()))
	}
}

// 同一 (dev, topic) 的新指令必须把未确认的旧指令作废。
// 否则设备离线时先 pair 再 unpair，上线后两条并行重投，最终状态随机。
func TestNewCommandSupersedesOldOnSameTopic(t *testing.T) {
	tr, pub, pres, _, clock := newTracker(t)
	pres.set("d", true)
	tr.Send("d", "printer/d/pair", []byte(`{"paired":true}`), true)
	if tr.Pending("d") != 1 {
		t.Fatal("第一条没记上")
	}
	cid2, _ := tr.Send("d", "printer/d/pair", []byte(`{"paired":false}`), true)

	if n := tr.Pending("d"); n != 1 {
		t.Errorf("同 topic 下应当只剩最新一条，实际 %d", n)
	}
	// 旧的那条被记成 superseded。
	if o, ok := tr.LastOutcome("d"); !ok || o.Err != "superseded" {
		t.Errorf("旧指令结局 = %+v", o)
	}
	// 重投的只能是新的那条。
	*clock = clock.Add(retryBase + time.Second)
	tr.sweep()
	msgs := pub.all()
	if cidOf(t, msgs[len(msgs)-1].payload) != cid2 {
		t.Error("重投的不是最新那条")
	}

	// 不同 topic 之间互不影响。
	tr.Send("d", "printer/d/usage", []byte(`{"now":1}`), false)
	if n := tr.Pending("d"); n != 2 {
		t.Errorf("不同 topic 应当并存，实际 %d", n)
	}
}

// 离线设备不消耗重投次数，光靠次数上限这条记录永不结案，内存会一直涨。
func TestPendingExpiresByTTL(t *testing.T) {
	tr, pub, pres, _, clock := newTracker(t)
	pres.set("d", false)
	tr.Send("d", "printer/d/usage", []byte(`{"now":1}`), false)

	*clock = clock.Add(pendingTTL - time.Minute)
	tr.sweep()
	if tr.Pending("d") != 1 {
		t.Fatal("没到期就被清了")
	}
	*clock = clock.Add(2 * time.Minute)
	tr.sweep()
	if tr.Pending("d") != 0 {
		t.Error("到期没清")
	}
	if o, ok := tr.LastOutcome("d"); !ok || o.OK || o.Err != "expired" {
		t.Errorf("结局 = %+v", o)
	}
	if len(pub.all()) != 1 {
		t.Errorf("离线期间不该重投，共 %d 条", len(pub.all()))
	}
}

// 旧固件不回 ACK：放弃一次之后就不再跟踪它，否则每条下行投 4 次、
// 每次刷一条 ERROR，真正的告警会被淹掉。
func TestStopsTrackingDeviceThatNeverAcks(t *testing.T) {
	tr, pub, pres, _, clock := newTracker(t)
	pres.set("old", true)
	tr.Send("old", "printer/old/usage", []byte(`{"now":1}`), false)
	for i := 0; i < maxTries+2; i++ {
		*clock = clock.Add(retryMax + time.Second)
		tr.sweep()
	}
	if n := len(pub.all()); n != maxTries {
		t.Fatalf("第一条共投 %d 次", n)
	}

	// 之后的下行只发一次，不跟踪。
	before := len(pub.all())
	tr.Send("old", "printer/old/usage", []byte(`{"now":2}`), false)
	if tr.Pending("old") != 0 {
		t.Error("已知不回 ACK 的设备不该再被跟踪")
	}
	for i := 0; i < maxTries+2; i++ {
		*clock = clock.Add(retryMax + time.Second)
		tr.sweep()
	}
	if n := len(pub.all()) - before; n != 1 {
		t.Errorf("第二条发了 %d 次，期望 1 次", n)
	}
}

// 刷了新固件开始回 ACK 之后，要能自己恢复跟踪——标记不能是单向的。
func TestResumesTrackingWhenDeviceStartsAcking(t *testing.T) {
	tr, _, pres, _, clock := newTracker(t)
	pres.set("d", true)
	tr.Send("d", "printer/d/usage", []byte(`{"now":1}`), false)
	for i := 0; i < maxTries+2; i++ {
		*clock = clock.Add(retryMax + time.Second)
		tr.sweep()
	}
	cid, _ := tr.Send("d", "printer/d/usage", []byte(`{"now":2}`), false)
	if tr.Pending("d") != 0 {
		t.Fatal("此时不该跟踪")
	}
	// 设备升级了，对那条未跟踪的指令回了 ACK。
	tr.Ack("d", cid, true, "")

	tr.Send("d", "printer/d/usage", []byte(`{"now":3}`), false)
	if tr.Pending("d") != 1 {
		t.Error("设备开始回 ACK 之后没有恢复跟踪")
	}
}

// 结局里要带真实 cid，排障时才查得到是哪条。
func TestOutcomeCarriesCID(t *testing.T) {
	tr, _, pres, _, _ := newTracker(t)
	pres.set("d", true)
	cid, _ := tr.Send("d", "printer/d/usage", []byte(`{"now":1}`), false)
	tr.Ack("d", cid, true, "")
	if o, _ := tr.LastOutcome("d"); o.CID != cid {
		t.Errorf("Outcome.CID = %q，期望 %q", o.CID, cid)
	}
}

// —— 审计第二轮：作废机制的两个漏洞 ——

// noAck 分支也必须作废旧指令。
// 场景：pair(A) 还挂着重投 → 设备因 usage 连丢被标成 noAck → 发 pair(C)
// 只发一次不跟踪 → A 照常重投，设备最后应用的是旧的 A。
func TestNoAckDeviceStillSupersedes(t *testing.T) {
	tr, pub, pres, _, clock := newTracker(t)
	pres.set("d", true)

	// 先挂一条 pair。
	tr.Send("d", "printer/d/pair", []byte(`{"paired":true}`), true)
	// 再用 usage 把这台设备熬成 noAck。
	tr.Send("d", "printer/d/usage", []byte(`{"now":1}`), false)
	for i := 0; i < maxTries+2; i++ {
		*clock = clock.Add(retryMax + time.Second)
		tr.sweep()
	}
	tr.mu.Lock()
	marked := tr.noAck["d"]
	tr.mu.Unlock()
	if !marked {
		t.Fatal("设备没被标成 noAck，场景没复现")
	}

	// 现在发一条新的 pair：虽然不跟踪，也必须把旧的那条作废掉。
	before := len(pub.all())
	tr.Send("d", "printer/d/pair", []byte(`{"paired":false}`), true)

	for i := 0; i < maxTries+2; i++ {
		*clock = clock.Add(retryMax + time.Second)
		tr.sweep()
	}
	for _, m := range pub.all()[before+1:] {
		if m.topic == "printer/d/pair" {
			t.Fatalf("新 pair 之后旧 pair 还在重投：%s", m.payload)
		}
	}
}

// sweep 的重投不能越过 Send：被作废的 cid 绝不能在作废它的那条之后发出。
// pair 是 retain=1，旧 payload 后发会把 broker 上的保留消息盖回旧状态。
func TestSupersededCommandNeverPublishedAfterSuccessor(t *testing.T) {
	pub := &fakePub{}
	pres := &fakePresence{online: map[string]bool{"d": true}}
	tr := New(pub, pres)

	// 真实时钟 + 极短退避，让 sweep 每次都想重投。
	var mu sync.Mutex
	var sends []string // 按发布顺序记下 cid

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // 不停重投
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				// 持续制造重投压力：清掉 noAck、把次数退回去、让记录立刻
				// 到期。否则设备几轮就被熬成 noAck，之后没有任何重投，
				// 这个测试会变成空测。
				tr.mu.Lock()
				delete(tr.noAck, "d")
				for _, p := range tr.pend {
					p.next = time.Time{}
					p.tries = 1
				}
				tr.mu.Unlock()
				tr.sweep()
			}
		}
	}()
	go func() { // 不停发新指令
		defer wg.Done()
		for i := 0; i < 200; i++ {
			cid, err := tr.Send("d", "printer/d/pair",
				[]byte(`{"paired":true}`), true)
			if err == nil {
				mu.Lock()
				sends = append(sends, cid)
				mu.Unlock()
			}
		}
	}()
	go func() { time.Sleep(300 * time.Millisecond); close(done) }()
	wg.Wait()

	// 从发布记录里还原每个 cid 最后一次出现的位置。
	order := map[string]int{}
	for i, m := range pub.all() {
		order[cidOf(t, m.payload)] = i
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(sends); i++ {
		prev, cur := sends[i-1], sends[i]
		pi, ok1 := order[prev]
		ci, ok2 := order[cur]
		if ok1 && ok2 && pi > ci {
			t.Fatalf("被取代的 %s 在取代它的 %s 之后才发出（%d > %d）",
				prev, cur, pi, ci)
		}
	}
	if len(sends) < 10 {
		t.Fatalf("只发了 %d 条，压力不够", len(sends))
	}
}

// 发布失败时不能把旧记录也赔进去——否则这条指令整个丢了，两端照样分叉。
func TestFailedSendKeepsOldPending(t *testing.T) {
	tr, pub, pres, _, _ := newTracker(t)
	pres.set("d", true)
	oldCID, _ := tr.Send("d", "printer/d/pair", []byte(`{"paired":true}`), true)

	pub.mu.Lock()
	pub.err = errSentinel
	pub.mu.Unlock()
	if _, err := tr.Send("d", "printer/d/pair", []byte(`{"paired":false}`), true); err == nil {
		t.Fatal("应当返回发布错误")
	}
	if tr.Pending("d") != 1 {
		t.Errorf("待确认 = %d，旧记录应当还在", tr.Pending("d"))
	}
	tr.mu.Lock()
	_, still := tr.pend[oldCID]
	tr.mu.Unlock()
	if !still {
		t.Error("发布失败却把旧记录删了，这条指令整个丢了")
	}
}
