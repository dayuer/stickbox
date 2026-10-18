package broker

import (
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

// pending 不能顶掉在线 active；active 可以接管 pending。
func TestIDTableRejectsPendingTakeover(t *testing.T) {
	tab := newIDTable()
	activeCl, pendingCl := &mqtt.Client{}, &mqtt.Client{}
	active := auth.Identity{Dev: "d1", Role: auth.RoleDevice}
	pending := auth.Identity{Dev: "d1", Role: auth.RoleDevice, Pending: true}

	if !tab.admit(activeCl, active) {
		t.Fatal("active 应能连接")
	}
	if tab.admit(pendingCl, pending) {
		t.Fatal("active 在线时 pending 竟然被放行")
	}
	tab.drop(activeCl)
	if !tab.admit(pendingCl, pending) {
		t.Fatal("active 下线后 pending 应放行")
	}
	if !tab.admit(&mqtt.Client{}, active) {
		t.Fatal("active 应能接管 pending")
	}
}

func TestPendingLimiter(t *testing.T) {
	l := newPendingLimiter(6, time.Minute)
	now := time.Unix(1000, 0)
	for i := 0; i < 6; i++ {
		if !l.allow("d1", now) {
			t.Fatalf("第 %d 次 pending 连接被限流", i+1)
		}
	}
	if l.allow("d1", now) {
		t.Fatal("第 7 次 pending 连接应被限流")
	}
	if !l.allow("d1", now.Add(time.Minute+time.Second)) {
		t.Fatal("窗口过后应重新放行")
	}
	if !l.allow("d2", now) {
		t.Fatal("不同 dev 不应互相影响")
	}
}

type fakeRouter struct{ seen []string }

func (f *fakeRouter) Seen(dev string) { f.seen = append(f.seen, dev) }

func publishStatus(h *Hook, topic string, payload []byte) {
	h.OnPublished(nil, packets.Packet{TopicName: topic, Payload: payload})
}

// 设备心跳只要到达 status 主题就记在线；payload 不参与判断。
func TestStatusHeartbeatMarksSeen(t *testing.T) {
	r := &fakeRouter{}
	h := &Hook{router: r}
	publishStatus(h, "printer/f412fa87c9e0/status", []byte(`{"state":"ready","heap":51234}`))

	if len(r.seen) != 1 || r.seen[0] != "f412fa87c9e0" {
		t.Fatalf("在线记录 = %v，期望 [f412fa87c9e0]", r.seen)
	}
}

// payload 畸形也不影响在线判断：能发到经 ACL 校验的主题，就说明连接活着。
func TestStatusIgnoresPayloadContent(t *testing.T) {
	r := &fakeRouter{}
	h := &Hook{router: r}
	publishStatus(h, "printer/f412fa87c9e0/status", []byte("not json at all"))
	if len(r.seen) != 1 || r.seen[0] != "f412fa87c9e0" {
		t.Fatalf("在线记录 = %v，期望 [f412fa87c9e0]", r.seen)
	}
}

// topic 里的 dev 是权威，payload 里的 dev 是设备自报的，不能信。
func TestStatusTrustsTopicNotPayload(t *testing.T) {
	r := &fakeRouter{}
	h := &Hook{router: r}
	publishStatus(h, "printer/f412fa87c9e0/status", []byte(`{"dev":"aabbccddeeff"}`))
	if len(r.seen) != 1 || r.seen[0] != "f412fa87c9e0" {
		t.Fatalf("记录 = %v，必须以 topic 为准", r.seen)
	}
}

// 除了 status，其它叶子主题都不再参与在线判断。
func TestNonStatusTopicsIgnored(t *testing.T) {
	r := &fakeRouter{}
	h := &Hook{router: r}
	for _, topic := range []string{
		"printer/f412fa87c9e0/job", "printer/f412fa87c9e0/profile", "printer/f412fa87c9e0/usage",
		"printer/f412fa87c9e0/ident", "printer/f412fa87c9e0/cmd",
	} {
		publishStatus(h, topic, []byte(`{}`))
	}
	if len(r.seen) != 0 {
		t.Errorf("非 status 主题记了在线：%v", r.seen)
	}
}

func TestBadTopicIgnored(t *testing.T) {
	r := &fakeRouter{}
	h := &Hook{router: r}
	for _, topic := range []string{"status", "printer/status", "x/aaa/status",
		"printer/f412fa87c9e0/status/x", "printer/+/status"} {
		publishStatus(h, topic, []byte(`{}`))
	}
	if len(r.seen) != 0 {
		t.Errorf("非法 topic 记了在线：%v", r.seen)
	}
}

// 会话接管：同一个 client id 重连时，旧连接的 OnDisconnect 在新连接建立
// **之后**才触发。用 client id 做键的话，那次清理会把新连接的身份一起删掉，
// 此后每次 ACL 检查都查不到身份、一律拒绝——设备连自己的状态都发不出去，
// 断开重连成死循环。这个 bug 在实机上现形过。
func TestIdentitySurvivesSessionTakeover(t *testing.T) {
	tbl := newIDTable()
	id := auth.Identity{Dev: "aaa", Role: auth.RoleDevice}

	old := &mqtt.Client{ID: "aaa"}
	tbl.put(old, id)

	// 设备重连：新连接、同一个 client id
	fresh := &mqtt.Client{ID: "aaa"}
	tbl.put(fresh, id)

	// 旧连接这时才收到 OnDisconnect
	tbl.drop(old)

	if _, ok := tbl.get(fresh); !ok {
		t.Fatal("旧连接的清理把新连接的身份删掉了——ACL 会全部拒绝")
	}
}

func TestIdentityDroppedOnDisconnect(t *testing.T) {
	tbl := newIDTable()
	cl := &mqtt.Client{ID: "aaa"}
	tbl.put(cl, auth.Identity{Dev: "aaa", Role: auth.RoleDevice})
	tbl.drop(cl)
	if _, ok := tbl.get(cl); ok {
		t.Error("断开后身份没清掉")
	}
}

type fakePhotoSink struct {
	dev   string
	st    PhotoStatus
	calls int
}

func (f *fakePhotoSink) UpdatePhotoStatus(dev string, st PhotoStatus) {
	f.dev, f.st, f.calls = dev, st, f.calls+1
}

func TestStatusPhotoParsed(t *testing.T) {
	sink := &fakePhotoSink{}
	h := &Hook{router: &fakeRouter{}, photo: sink}
	payload := []byte(`{"photo":{"cap":true,"ready":true,"plv":7,"have":12,
		"bad":["a1b2c3d4ee","b1","c1","d1","e1","f1","01","02","03"],
		"free_kb":1234,"state":"ok","init_done":"abcdef01"}}`)
	publishStatus(h, "printer/f412fa87c9e0/status", payload)
	if sink.calls != 1 || sink.dev != "f412fa87c9e0" {
		t.Fatalf("sink = %+v", sink)
	}
	st := sink.st
	if !st.Cap || !st.Ready || st.PLV != 7 || st.Have != 12 ||
		st.FreeKB != 1234 || st.State != "ok" || st.InitDone != "abcdef01" {
		t.Fatalf("状态 = %+v", st)
	}
	if len(st.Bad) != 8 || st.Bad[0] != "a1b2c3d4" || st.Bad[7] != "02" {
		t.Fatalf("bad = %v", st.Bad)
	}
}

func TestStatusWithoutPhotoIgnored(t *testing.T) {
	sink := &fakePhotoSink{}
	h := &Hook{router: &fakeRouter{}, photo: sink}
	publishStatus(h, "printer/f412fa87c9e0/status", []byte(`{"state":"ready"}`))
	publishStatus(h, "printer/f412fa87c9e0/status", []byte(`{"photo":"bad"}`))
	if sink.calls != 0 {
		t.Fatalf("不该调用 sink：%+v", sink)
	}
}
