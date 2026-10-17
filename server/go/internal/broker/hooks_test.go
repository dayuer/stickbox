package broker

import (
	"encoding/json"
	"testing"

	mqtt "github.com/mochi-mqtt/server/v2"

	"github.com/dayuer/stickbox/server/go/internal/auth"

	"github.com/dayuer/stickbox/server/go/internal/device"
)

type capturedMsg struct {
	dev string
	msg device.Msg
}

type fakeRouter struct{ got []capturedMsg }

func (f *fakeRouter) Send(dev string, m device.Msg) {
	f.got = append(f.got, capturedMsg{dev, m})
}

func TestRouteStatusHeartbeat(t *testing.T) {
	r := &fakeRouter{}
	payload, _ := json.Marshal(map[string]any{
		"dev": "aaa", "job": "", "state": "ready", "bytes": 0,
		"serial": "PA",
		"prn":    map[string]any{"display": "Ready"},
	})
	routeStatus(r, "printer/aaa/status", payload)

	if len(r.got) != 1 {
		t.Fatalf("投递了 %d 条", len(r.got))
	}
	m := r.got[0].msg
	if m.Kind != device.KindHeartbeat || m.Serial != "PA" {
		t.Errorf("消息 = %+v，期望 Heartbeat + serial=PA", m)
	}
	if len(m.Printer) == 0 {
		t.Error("prn 原文没带上，/api/status 就没东西可显示")
	}
}

// done 必须带 job 字段，否则服务端要等 180 秒超时才知道这件结束了。
func TestRouteStatusDone(t *testing.T) {
	r := &fakeRouter{}
	routeStatus(r, "printer/aaa/status",
		[]byte(`{"dev":"aaa","job":"j1","state":"done","bytes":4096,"serial":"PA"}`))

	m := r.got[0].msg
	if m.Kind != device.KindJobDone || m.JobID != "j1" || m.Bytes != 4096 {
		t.Errorf("消息 = %+v", m)
	}
}

func TestRouteStatusFailedCarriesError(t *testing.T) {
	r := &fakeRouter{}
	routeStatus(r, "printer/aaa/status",
		[]byte(`{"dev":"aaa","job":"j1","state":"failed","err":"USB 超时"}`))
	m := r.got[0].msg
	if m.Kind != device.KindJobFailed || m.Err != "USB 超时" {
		t.Errorf("消息 = %+v", m)
	}
}

func TestRouteStatusDownloading(t *testing.T) {
	r := &fakeRouter{}
	routeStatus(r, "printer/aaa/status",
		[]byte(`{"dev":"aaa","job":"j1","state":"downloading","serial":"PA"}`))
	if m := r.got[0].msg; m.Kind != device.KindDownloading || m.JobID != "j1" {
		t.Errorf("消息 = %+v", m)
	}
}

// LWT：设备掉线时 broker 代发，serial 要清空。
func TestRouteStatusOffline(t *testing.T) {
	r := &fakeRouter{}
	routeStatus(r, "printer/aaa/status", []byte(`{"dev":"aaa","state":"offline"}`))
	m := r.got[0].msg
	if m.Kind != device.KindHeartbeat || m.Serial != "" {
		t.Errorf("消息 = %+v，offline 时 serial 应为空", m)
	}
}

// 畸形 JSON 不能让 broker 崩，也不能投出垃圾消息。
func TestRouteStatusIgnoresGarbage(t *testing.T) {
	r := &fakeRouter{}
	routeStatus(r, "printer/aaa/status", []byte("not json at all"))
	if len(r.got) != 0 {
		t.Errorf("畸形 payload 投出了 %d 条消息", len(r.got))
	}
}

// topic 里的 dev 是权威，payload 里的 dev 是设备自报的，不能信。
func TestRouteStatusTrustsTopicNotPayload(t *testing.T) {
	r := &fakeRouter{}
	routeStatus(r, "printer/aaa/status", []byte(`{"dev":"bbb","state":"ready"}`))
	if len(r.got) != 1 || r.got[0].dev != "aaa" {
		t.Errorf("路由到了 %+v，必须以 topic 为准", r.got)
	}
}

func TestRouteStatusIgnoresBadTopic(t *testing.T) {
	r := &fakeRouter{}
	for _, topic := range []string{"status", "printer/status", "x/aaa/status"} {
		routeStatus(r, topic, []byte(`{"state":"ready"}`))
	}
	if len(r.got) != 0 {
		t.Errorf("非法 topic 投出了 %d 条消息", len(r.got))
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

// USB 事件计数随心跳上报（API 文档 3.6 的 usb 字段）。
//
// 多出来的字段不能让整条心跳解析失败——老固件不带它，新固件带，
// 两种都得能收。
func TestRouteStatusAcceptsUSBField(t *testing.T) {
	r := &fakeRouter{}
	payload := []byte(`{"dev":"aaa","job":"","state":"ready","bytes":0,` +
		`"serial":"PA","usb":{"att":3,"det":2,"err":"release=ESP_ERR_INVALID_STATE close=ESP_ERR_INVALID_STATE"}}`)
	routeStatus(r, "printer/aaa/status", payload)

	if len(r.got) != 1 {
		t.Fatalf("带 usb 字段的心跳没被投递，投了 %d 条", len(r.got))
	}
	if r.got[0].msg.Serial != "PA" {
		t.Errorf("serial = %q，usb 字段不该影响其它字段的解析", r.got[0].msg.Serial)
	}
}

// 老固件不带 usb 字段，照样要能收。
func TestRouteStatusWithoutUSBField(t *testing.T) {
	r := &fakeRouter{}
	routeStatus(r, "printer/aaa/status",
		[]byte(`{"dev":"aaa","job":"","state":"ready","bytes":0,"serial":"PA"}`))
	if len(r.got) != 1 {
		t.Fatal("不带 usb 字段的心跳应当照常投递")
	}
}

// 心跳里的 heap 要一路带到 actor。
//
// 这条是为一次误诊写的：设备堆耗尽时 mbedtls 分不到大数缓冲，握手失败在
// 服务端日志里长得跟证书错误一模一样（tls: access denied / connection reset）。
// 设备一直在报 heap，服务端却把它丢了，于是只能靠串口——而用户现场的板子
// 串口够不着。丢掉这个字段等于把唯一的远程诊断依据丢掉。
func TestStatusCarriesHeap(t *testing.T) {
	r := &fakeRouter{}
	routeStatus(r, "printer/f412fa87c9e0/status",
		[]byte(`{"state":"idle","heap":51234,"serial":"CNB1S5N3S7"}`))

	if len(r.got) != 1 {
		t.Fatalf("发出 %d 条消息，期望 1 条", len(r.got))
	}
	if got := r.got[0].msg.Heap; got != 51234 {
		t.Errorf("Heap = %d，期望 51234——丢掉它就没法远程判断堆耗尽", got)
	}
}
