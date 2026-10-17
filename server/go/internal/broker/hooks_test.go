package broker

import (
	"testing"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

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
