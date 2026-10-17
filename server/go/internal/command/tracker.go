// Package command 给云端下发的 MQTT 指令加上「设备明确确认」这一层。
//
// 为什么需要它：MQTT 的 QoS1 只保证消息投递到设备的 MQTT 栈，不保证设备
// 真的应用了。解析失败、状态机拒绝、应用到一半掉电——这些情况服务端都以为
// 成功了，两端状态就此分叉，而且两头都不报错。帧包通知尤其要命：丢一条的
// 表现是电视永远停在旧画面，没人会发现。
//
// 做法：每条下行指令带一个 cid，设备**应用完**才回 printer/{dev}/ack。
// 服务端在收到 ACK 之前反复重投；设备回 ok=0 表示「刻意不应用」，那是永久
// 失败，立即停止重投并记录下来，不浪费重试预算。
package command

import (
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/rs/xid"
)

// Publisher 是 command 对 MQTT 的窄依赖。
type Publisher interface {
	PublishRaw(topic string, payload []byte, retain bool) error
}

// Presence 告诉 tracker 设备在不在线。离线时不重投——消息发出去也没人收，
// 白白烧掉重试预算，等上线再补发。为 nil 时视为一直在线。
type Presence interface {
	Online(dev string) bool
}

const (
	// 首次重投间隔，之后每次翻倍。
	retryBase = 15 * time.Second
	// 重投间隔上限。
	retryMax = 5 * time.Minute
	// 投这么多次还没 ACK 就放弃，把设备标成「状态未确认」。
	maxTries = 6
)

// ErrPayloadNotObject：payload 必须是 JSON 对象，才能塞得进 cid。
var ErrPayloadNotObject = errors.New("command: payload 不是 JSON 对象")

type pending struct {
	dev     string
	topic   string
	payload []byte
	retain  bool
	tries   int
	next    time.Time
}

// Outcome 是一条指令的最终结局，给运维看。
type Outcome struct {
	CID      string
	Topic    string
	OK       bool
	Err      string
	Tries    int
	Finished time.Time
}

type Tracker struct {
	pub      Publisher
	presence Presence
	now      func() time.Time

	mu      sync.Mutex
	pend    map[string]*pending // cid -> 未确认的指令
	last    map[string]Outcome  // dev -> 最近一条的结局
	stopped bool
	wake    chan struct{}
}

func New(pub Publisher, presence Presence) *Tracker {
	return &Tracker{
		pub: pub, presence: presence, now: time.Now,
		pend: map[string]*pending{},
		last: map[string]Outcome{},
		wake: make(chan struct{}, 1),
	}
}

// Send 给 payload 塞一个 cid 后发布，并把它记成「待确认」。
//
// retain 透传给 MQTT：pair 用 retain=1（设备离线时错过配对，重连要能拿到
// 当前状态），usage 用 retain=0（payload 里的 now 会过期）。retain 和本包的
// 重投是两件事——retain 解决「重连拿到最后状态」，重投解决「设备收到了但
// 没应用」。
func (t *Tracker) Send(dev, topic string, payload []byte, retain bool) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return "", ErrPayloadNotObject
	}
	cid := xid.New().String()
	raw, _ := json.Marshal(cid)
	obj["cid"] = raw
	withCID, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}

	t.mu.Lock()
	t.pend[cid] = &pending{
		dev: dev, topic: topic, payload: withCID, retain: retain,
		tries: 1, next: t.now().Add(retryBase),
	}
	t.mu.Unlock()

	if err := t.pub.PublishRaw(topic, withCID, retain); err != nil {
		t.mu.Lock()
		delete(t.pend, cid)
		t.mu.Unlock()
		return "", err
	}
	t.nudge()
	return cid, nil
}

// Ack 处理设备回执。ok=false 是永久失败（设备刻意不应用，重投还会是同样
// 结果），直接结案，不再重投。
func (t *Tracker) Ack(dev, cid string, ok bool, errMsg string) {
	t.mu.Lock()
	p, found := t.pend[cid]
	if found {
		delete(t.pend, cid)
	}
	t.mu.Unlock()

	if !found {
		// 设备对一条我们已经结案的指令补了 ACK（重投撞上在途回执），
		// 或者 cid 根本不是我们发的。记一笔即可，不是错误。
		slog.Debug("收到未知 cid 的 ACK", "dev", dev, "cid", cid, "ok", ok)
		return
	}
	t.finish(p, ok, errMsg)
	if ok {
		slog.Info("指令已确认", "dev", dev, "topic", p.topic, "cid", cid, "tries", p.tries)
	} else {
		slog.Warn("设备拒绝执行指令", "dev", dev, "topic", p.topic, "cid", cid, "err", errMsg)
	}
}

func (t *Tracker) finish(p *pending, ok bool, errMsg string) {
	t.mu.Lock()
	t.last[p.dev] = Outcome{
		CID: "", Topic: p.topic, OK: ok, Err: errMsg,
		Tries: p.tries, Finished: t.now(),
	}
	t.mu.Unlock()
}

// Pending 返回某设备还有多少条指令没被确认。0 表示两端状态一致。
func (t *Tracker) Pending(dev string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, p := range t.pend {
		if p.dev == dev {
			n++
		}
	}
	return n
}

// LastOutcome 返回某设备最近一条指令的结局，给 /api 和排障用。
func (t *Tracker) LastOutcome(dev string) (Outcome, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	o, ok := t.last[dev]
	return o, ok
}

func (t *Tracker) online(dev string) bool {
	if t.presence == nil {
		return true
	}
	return t.presence.Online(dev)
}

func (t *Tracker) nudge() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// Run 跑重投循环，直到 ctx 结束。调用方起一个 goroutine。
func (t *Tracker) Run(done <-chan struct{}) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.wake:
		case <-tick.C:
		}
		t.sweep()
	}
}

// sweep 重投所有到期且设备在线的未确认指令。
func (t *Tracker) sweep() {
	now := t.now()

	type job struct {
		p   *pending
		cid string
	}
	var jobs []job
	var dead []job

	t.mu.Lock()
	for cid, p := range t.pend {
		if now.Before(p.next) || !t.online(p.dev) {
			continue
		}
		if p.tries >= maxTries {
			delete(t.pend, cid)
			dead = append(dead, job{p, cid})
			continue
		}
		p.tries++
		back := retryBase * time.Duration(1<<uint(p.tries-1))
		if back > retryMax {
			back = retryMax
		}
		p.next = now.Add(back)
		jobs = append(jobs, job{p, cid})
	}
	t.mu.Unlock()

	for _, j := range dead {
		// 投了 maxTries 次都没回执。设备可能一直在线却从不 ACK（固件太旧
		// 或坏了），这时两端状态就是不一致的，必须让人看得见，不能静默。
		t.finish(j.p, false, "no_ack")
		slog.Error("指令始终未被确认，设备状态未知",
			"dev", j.p.dev, "topic", j.p.topic, "cid", j.cid, "tries", j.p.tries)
	}
	for _, j := range jobs {
		if err := t.pub.PublishRaw(j.p.topic, j.p.payload, j.p.retain); err != nil {
			slog.Warn("重投失败", "dev", j.p.dev, "topic", j.p.topic, "err", err)
			continue
		}
		slog.Info("重投未确认指令", "dev", j.p.dev, "topic", j.p.topic,
			"cid", j.cid, "tries", j.p.tries)
	}
}
