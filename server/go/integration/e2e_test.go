package integration

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/frame"
)

type deviceUsage struct {
	Now      int64 `json:"now"`
	TS       int64 `json:"ts"`
	FiveHour *struct {
		UsedPercentage float64 `json:"used_percentage"`
		ResetsAt       int64   `json:"resets_at"`
	} `json:"five_hour"`
	SevenDay *struct {
		UsedPercentage float64 `json:"used_percentage"`
		ResetsAt       int64   `json:"resets_at"`
	} `json:"seven_day"`
}

func usageReport(now time.Time, pct float64) []byte {
	b, _ := json.Marshal(map[string]any{
		"five_hour": map[string]any{
			"used_percentage": pct,
			"resets_at":       now.Add(time.Hour).Unix(),
		},
		"seven_day": map[string]any{
			"used_percentage": pct / 2,
			"resets_at":       now.Add(48 * time.Hour).Unix(),
		},
		"limits": []map[string]any{{"kind": "weekly_all", "percent": pct / 2}},
	})
	return b
}

func parseUsage(t *testing.T, raw []byte) deviceUsage {
	t.Helper()
	var u deviceUsage
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatalf("设备收到的用量不是合法 JSON：%v（%s）", err, raw)
	}
	return u
}

func pairPayload(t *testing.T, raw []byte) bool {
	t.Helper()
	var p struct {
		Paired bool `json:"paired"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("配对通知不是合法 JSON：%v（%s）", err, raw)
	}
	return p.Paired
}

// 完整链路：设备注册 → 出码 → 设备连 MQTT 等配对 → agent 输码 →
// 设备收到 paired=true → 用新身份重连 → agent 上报用量 → 设备收到。
//
// 待配对密钥按 ACL 不能订阅 usage；服务端在配对成功后会踢掉旧连接，
// 固件自动重连后用 active 身份重新订阅。这里模拟那次重连。
func TestEndToEndRegisterPairUsage(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")

	dev := e.connectDevice("f412fa87c9e0", devKey)
	dev.heartbeat()
	if err := dev.subscribePair(); err != nil {
		t.Fatalf("订阅配对 = %v", err)
	}
	if err := dev.subscribeUsage(); err == nil {
		t.Fatal("pending 密钥订阅了 usage——ACL 没生效")
	}

	agentTok, pairedDev, _ := e.pair(code, "")
	if pairedDev != "f412fa87c9e0" {
		t.Fatalf("配对的 dev = %q", pairedDev)
	}

	// 配对后服务端立即踢掉旧连接（旧身份还是 pending）；重连后用 active
	// 身份订阅，retained 的 paired=true 会补发。
	time.Sleep(500 * time.Millisecond)
	dev.disconnect()
	dev = e.connectDevice("f412fa87c9e0", devKey)
	if err := dev.subscribePair(); err != nil {
		t.Fatalf("配对后订阅配对 = %v", err)
	}
	if !pairPayload(t, dev.waitPair(5*time.Second)) {
		t.Error("配对通知不是 paired=true")
	}
	if err := dev.subscribeUsage(); err != nil {
		t.Fatalf("配对后订阅用量 = %v", err)
	}

	before := time.Now().Unix()
	e.reportUsage(agentTok, usageReport(time.Now(), 12.5))
	got := parseUsage(t, dev.waitUsage(5*time.Second))
	after := time.Now().Unix()

	if got.FiveHour == nil || got.FiveHour.UsedPercentage != 12.5 {
		t.Errorf("five_hour = %+v，期望 12.5", got.FiveHour)
	}
	if got.Now < before || got.Now > after {
		t.Errorf("now = %d，不在 [%d,%d]", got.Now, before, after)
	}
	if got.TS < before || got.TS > after {
		t.Errorf("ts = %d，不在 [%d,%d]", got.TS, before, after)
	}

	// agent 能查到自己的设备和用量。
	st, b := e.req("GET", "/api/agent/devices", "",
		map[string]string{"Authorization": "Bearer " + agentTok})
	if st != 200 {
		t.Fatalf("agent devices = %d %s", st, b)
	}
	var list struct {
		Devices []struct {
			Dev    string `json:"dev"`
			Online bool   `json:"online"`
		} `json:"devices"`
	}
	json.Unmarshal(b, &list)
	if len(list.Devices) != 1 || !list.Devices[0].Online {
		t.Errorf("设备列表 = %s", b)
	}
	st, b = e.req("GET", "/api/agent/usage", "",
		map[string]string{"Authorization": "Bearer " + agentTok})
	if st != 200 {
		t.Errorf("agent usage = %d %s", st, b)
	}
}

// usage 没有 retain：设备晚订阅时靠 OnSubscribed 补发，now 现场盖。
func TestEndToEndUsageResendOnSubscribe(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")
	agentTok, _, _ := e.pair(code, "")

	// 设备还没连；agent 先上报。配对后注册时那把密钥已经是有效密钥。
	e.reportUsage(agentTok, usageReport(time.Now(), 33))
	time.Sleep(1200 * time.Millisecond)

	dev := e.connectDevice("f412fa87c9e0", devKey)
	if err := dev.subscribeUsage(); err != nil {
		t.Fatalf("订阅 = %v", err)
	}
	got := parseUsage(t, dev.waitUsage(5*time.Second))
	if got.FiveHour == nil || got.FiveHour.UsedPercentage != 33 {
		t.Errorf("补发内容 = %+v", got.FiveHour)
	}
	if got.Now <= got.TS {
		t.Errorf("now=%d 不晚于 ts=%d，补发没有现场盖时间", got.Now, got.TS)
	}
}

// 已绑定的 MAC 重新注册：别人 409，原主人转正，旧密钥作废。
func TestEndToEndReRegisterOwnership(t *testing.T) {
	e := startServer(t)
	oldKey, code1 := e.register("f412fa87c9e0")
	agentA, _, _ := e.pair(code1, "")

	// 另一个用户 B。
	_, codeB := e.register("aabbccddeeff")
	agentB, _, _ := e.pair(codeB, "")

	newKey, code2 := e.register("f412fa87c9e0")
	if st, b := e.tryPair(code2, agentB); st != 409 {
		t.Fatalf("别人配对 = %d，期望 409：%s", st, b)
	}
	if st, b := e.tryPair(code2, agentA); st != 200 {
		t.Fatalf("原主人配对 = %d %s", st, b)
	}

	if _, err := e.tryConnectDevice("f412fa87c9e0", oldKey); err == nil {
		t.Error("旧设备密钥仍能连 MQTT")
	}
	d, err := e.tryConnectDevice("f412fa87c9e0", newKey)
	if err != nil {
		t.Errorf("新设备密钥连不上：%v", err)
	} else {
		d.disconnect()
	}
}

// 解绑：设备收到 paired=false，旧密钥失效，可重新注册出码。
func TestEndToEndUnpair(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")
	dev := e.connectDevice("f412fa87c9e0", devKey)
	if err := dev.subscribePair(); err != nil {
		t.Fatalf("订阅配对 = %v", err)
	}

	agentTok, pairedDev, _ := e.pair(code, "")
	// 配对后旧连接被踢；重连拿 retained 的 paired=true。
	time.Sleep(500 * time.Millisecond)
	dev.disconnect()
	dev = e.connectDevice("f412fa87c9e0", devKey)
	if err := dev.subscribePair(); err != nil {
		t.Fatalf("重连订阅配对 = %v", err)
	}
	if !pairPayload(t, dev.waitPair(5*time.Second)) {
		t.Fatal("没收到 paired=true")
	}
	e.unpair(agentTok, pairedDev)
	if pairPayload(t, dev.waitPair(5*time.Second)) {
		t.Error("解绑通知不是 paired=false")
	}

	if _, err := e.tryConnectDevice("f412fa87c9e0", devKey); err == nil {
		t.Error("解绑后旧设备密钥仍能连 MQTT")
	}
	_, newCode := e.register("f412fa87c9e0")
	if len(newCode) != 6 {
		t.Error("解绑后没有重新出码")
	}
}

// ACL：设备不能订阅别人的 usage / pair。
func TestEndToEndACLBlocksCrossDevice(t *testing.T) {
	e := startServer(t)
	devKey, _ := e.register("f412fa87c9e0")
	dev := e.connectDevice("f412fa87c9e0", devKey)

	for _, topic := range []string{
		"printer/aabbccddeeff/usage",
		"printer/aabbccddeeff/pair",
		"printer/aabbccddeeff/frame",
	} {
		if err := dev.subscribe(topic); err == nil {
			t.Errorf("设备订阅了 %s——ACL 没生效", topic)
		}
	}
}

func TestEndToEndRejectsBadKey(t *testing.T) {
	e := startServer(t)
	if _, err := e.tryConnectDevice("f412fa87c9e0", "bogus.key"); err == nil {
		t.Error("错误密钥竟然连上了")
	}
}

func TestEndToEndRejectsMismatchedUsername(t *testing.T) {
	e := startServer(t)
	devKey, _ := e.register("f412fa87c9e0")
	if _, err := e.tryConnectDevice("aabbccddeeff", devKey); err == nil {
		t.Error("用别的 username 拿着 A 的密钥连上了")
	}
}

// agent 只走 HTTP，不能连 MQTT。
func TestEndToEndAgentCannotConnectMQTT(t *testing.T) {
	e := startServer(t)
	_, code := e.register("f412fa87c9e0")
	agentTok, _, _ := e.pair(code, "")
	if _, err := e.tryConnectDevice("f412fa87c9e0", agentTok); err == nil {
		t.Error("agent 令牌连上了 MQTT")
	}
}

// 知道 MAC 的人可以注册一把 pending 密钥，但它只能收配对通知：
// 不能读 usage / frame，也不能影响原主人的密钥。
func TestEndToEndPendingKeyCannotReadUserData(t *testing.T) {
	e := startServer(t)
	ownerKey, code := e.register("f412fa87c9e0")
	agentTok, _, _ := e.pair(code, "")

	// 同一 MAC 再注册：新增一把 pending 密钥，原有效密钥不受影响。
	pendingKey, _ := e.register("f412fa87c9e0")
	p := e.connectDevice("f412fa87c9e0", pendingKey)
	if err := p.subscribePair(); err != nil {
		t.Fatalf("pending 应能读配对通知：%v", err)
	}
	if err := p.subscribeUsage(); err == nil {
		t.Error("pending 密钥订阅了 usage——ACL 没生效")
	}
	if err := p.subscribe("printer/f412fa87c9e0/frame"); err == nil {
		t.Error("pending 密钥订阅了 frame——ACL 没生效")
	}

	// 原主人的密钥仍能正常收用量。
	owner := e.connectDevice("f412fa87c9e0", ownerKey)
	if err := owner.subscribeUsage(); err != nil {
		t.Fatalf("原密钥订阅 usage：%v", err)
	}
	e.reportUsage(agentTok, usageReport(time.Now(), 17))
	got := parseUsage(t, owner.waitUsage(5*time.Second))
	if got.FiveHour == nil || got.FiveHour.UsedPercentage != 17 {
		t.Errorf("原主人收到 = %+v", got.FiveHour)
	}
	select {
	case b := <-p.usage:
		t.Errorf("pending 密钥收到了用量：%s", b)
	case <-time.After(500 * time.Millisecond):
	}
}

// 旧固件只在 MQTT_EVENT_CONNECTED 时订阅 usage，配对时密钥原地转正、
// 连接不断开就会一直停在配对页。服务端踢连接后，paho 自动重连并重订。
func TestEndToEndPairKicksLegacyDevice(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")

	legacy, err := e.connectLegacyDevice("f412fa87c9e0", devKey)
	if err != nil {
		t.Fatal(err)
	}
	// pending 时 usage 订阅必须被拒，不能出现 subscribed 信号。
	select {
	case <-legacy.subscribed:
		t.Fatal("pending 时 usage 订阅成功——ACL 没生效")
	case <-time.After(700 * time.Millisecond):
	}

	agentTok, pairedDev, _ := e.pair(code, "")
	if pairedDev != "f412fa87c9e0" {
		t.Fatalf("配对的 dev = %q", pairedDev)
	}

	// 服务端踢连接后，旧固件自动重连，OnConnect 里重新订阅 usage。
	select {
	case <-legacy.subscribed:
	case <-time.After(10 * time.Second):
		t.Fatal("配对后没有自动重连并重新订阅 usage")
	}

	e.reportUsage(agentTok, usageReport(time.Now(), 23))
	got := parseUsage(t, legacy.waitUsage(5*time.Second))
	if got.FiveHour == nil || got.FiveHour.UsedPercentage != 23 {
		t.Errorf("旧固件收到 = %+v", got.FiveHour)
	}
}

// 帧分发全链路：配对出帧 → 设备订阅收到版本 → 上报用量后版本前进 →
// GET 拿到帧包（meta 可解析） → ETag/304 → 重连订阅补发当前版本。
func TestEndToEndFrameDistribution(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")
	agentTok, _, _ := e.pair(code, "")

	dev := e.connectDevice("f412fa87c9e0", devKey)
	if err := dev.subscribeFrame(); err != nil {
		t.Fatalf("订阅 frame = %v", err)
	}
	ver1 := dev.waitFrameVer(10 * time.Second)

	e.reportUsage(agentTok, usageReport(time.Now(), 21.5))
	ver2 := dev.waitFrameVer(10 * time.Second)
	if ver2 <= ver1 {
		t.Fatalf("上报用量后版本没有前进：%d → %d", ver1, ver2)
	}

	st, body, etag := e.getFrame(devKey, "")
	if st != 200 {
		t.Fatalf("GET frame = %d %s", st, body)
	}
	if want := fmt.Sprintf(`"quota-%d"`, ver2); etag != want {
		t.Errorf("ETag = %q，期望 %q", etag, want)
	}
	if len(body) < 16 || string(body[:4]) != "SBF1" {
		t.Fatalf("帧包头部不对：%d 字节，%.4s", len(body), body)
	}
	bl := int(binary.BigEndian.Uint32(body[4:8]))
	al := int(binary.BigEndian.Uint32(body[8:12]))
	if 16+bl+al > len(body) {
		t.Fatalf("帧包长度非法：base=%d atlas=%d total=%d", bl, al, len(body))
	}
	meta, err := frame.ParseMeta(body[16+bl+al:])
	if err != nil {
		t.Fatalf("ParseMeta: %v", err)
	}
	if meta.V != frame.Version || meta.Band.MaxBandBytes == 0 {
		t.Errorf("meta = v%d max_band=%d", meta.V, meta.Band.MaxBandBytes)
	}

	st, body304, _ := e.getFrame(devKey, etag)
	if st != 304 || len(body304) != 0 {
		t.Errorf("If-None-Match = %d，body %d 字节", st, len(body304))
	}

	// 重连后订阅 frame，应补发当前版本（不依赖重新上报）。
	dev.disconnect()
	dev2 := e.connectDevice("f412fa87c9e0", devKey)
	if err := dev2.subscribeFrame(); err != nil {
		t.Fatalf("重连订阅 frame = %v", err)
	}
	if got := dev2.waitFrameVer(10 * time.Second); got != ver2 {
		t.Errorf("重连补发版本 = %d，期望 %d", got, ver2)
	}
}

// 合法设备在线时，拿到 MAC 的 pending 密钥不能顶号（会话接管 DoS）。
func TestEndToEndPendingCannotTakeOverActive(t *testing.T) {
	e := startServer(t)
	activeKey, code := e.register("f412fa87c9e0")
	agentTok, _, _ := e.pair(code, "")
	active := e.connectDevice("f412fa87c9e0", activeKey)
	if err := active.subscribeUsage(); err != nil {
		t.Fatalf("合法设备订阅 usage：%v", err)
	}

	pendingKey, _ := e.register("f412fa87c9e0")
	if _, err := e.tryConnectDevice("f412fa87c9e0", pendingKey); err == nil {
		t.Fatal("pending 密钥用同一个 client id 顶掉了在线设备")
	}

	// 合法设备仍在，能正常收到用量。
	e.reportUsage(agentTok, usageReport(time.Now(), 11))
	got := parseUsage(t, active.waitUsage(5*time.Second))
	if got.FiveHour == nil || got.FiveHour.UsedPercentage != 11 {
		t.Errorf("合法设备收到 = %+v", got.FiveHour)
	}
}

// 合法设备不在线时，pending 可以连上，但只能读 pair。
func TestEndToEndPendingAllowedWithoutActive(t *testing.T) {
	e := startServer(t)
	key, _ := e.register("f412fa87c9e0")
	p := e.connectDevice("f412fa87c9e0", key)
	if err := p.subscribePair(); err != nil {
		t.Fatalf("pending 应能读 pair：%v", err)
	}
	if err := p.subscribeUsage(); err == nil {
		t.Error("pending 订阅了 usage——ACL 没生效")
	}
}

// 合法设备重新上线时，可以接管在线的 pending 连接。
func TestEndToEndActiveTakesOverPending(t *testing.T) {
	e := startServer(t)
	activeKey, code := e.register("f412fa87c9e0")
	agentTok, _, _ := e.pair(code, "")
	pendingKey, _ := e.register("f412fa87c9e0")

	p := e.connectDevice("f412fa87c9e0", pendingKey)
	if err := p.subscribePair(); err != nil {
		t.Fatalf("pending 应能读 pair：%v", err)
	}

	// 同一个 client id，active 身份：允许接管。
	active := e.connectDevice("f412fa87c9e0", activeKey)
	if err := active.subscribeUsage(); err != nil {
		t.Fatalf("active 接管后订阅 usage：%v", err)
	}
	e.reportUsage(agentTok, usageReport(time.Now(), 13))
	got := parseUsage(t, active.waitUsage(5*time.Second))
	if got.FiveHour == nil || got.FiveHour.UsedPercentage != 13 {
		t.Errorf("接管后收到 = %+v", got.FiveHour)
	}
}

// 同一 dev 的 pending 连接每分钟最多 6 次。
func TestEndToEndPendingConnectRateLimited(t *testing.T) {
	e := startServer(t)
	key, _ := e.register("f412fa87c9e0")
	for i := 0; i < 6; i++ {
		d, err := e.tryConnectDevice("f412fa87c9e0", key)
		if err != nil {
			t.Fatalf("第 %d 次 pending 连接被拒：%v", i+1, err)
		}
		d.disconnect()
	}
	if _, err := e.tryConnectDevice("f412fa87c9e0", key); err == nil {
		t.Fatal("第 7 次 pending 连接没有被限流")
	}
}

// —— T18：下行指令必须拿到设备的明确确认 ——
//
// 热修之后配对流程多了一步：配对成功 → 服务端踢掉旧连接（旧身份是
// pending）→ 设备以转正身份重连 → 收到 retained 的 paired=true。
// pair 的 ACK 要在重连之后才发得出去——pending 密钥按 ACL 不能写 ack。

// pairAndReconnect 走完整流程，返回转正后已订阅 pair 的连接和 agent 令牌。
func pairAndReconnect(t *testing.T, e *env, dev, devKey, code string) (*fakeDevice, string) {
	t.Helper()
	d := e.connectDevice(dev, devKey)
	d.heartbeat()
	if err := d.subscribePair(); err != nil {
		t.Fatalf("订阅配对 = %v", err)
	}
	agentTok, _, _ := e.pair(code, "")

	time.Sleep(500 * time.Millisecond)
	d.disconnect()
	d = e.connectDevice(dev, devKey)
	if err := d.subscribePair(); err != nil {
		t.Fatalf("重连订阅配对 = %v", err)
	}
	return d, agentTok
}

// 设备转正重连后收到 retained 的 paired=true，确认它，服务端结案。
func TestEndToEndPairAckAfterReconnect(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")
	dev, _ := pairAndReconnect(t, e, "f412fa87c9e0", devKey, code)

	cid := cidOf(t, dev.waitPair(5*time.Second))
	dev.ack(cid, true, "")

	waitPendingZero(t, e, "f412fa87c9e0")
	if o, ok := e.cmd.LastOutcome("f412fa87c9e0"); !ok || !o.OK {
		t.Errorf("结局 = %+v ok=%v", o, ok)
	}
}

// 用量下行带 cid，设备确认后服务端结案、不再重投。
func TestEndToEndUsageAckClearsPending(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")
	dev, agentTok := pairAndReconnect(t, e, "f412fa87c9e0", devKey, code)
	dev.ack(cidOf(t, dev.waitPair(5*time.Second)), true, "")
	if err := dev.subscribeUsage(); err != nil {
		t.Fatalf("转正后订阅用量 = %v", err)
	}

	e.reportUsage(agentTok, usageReport(time.Now(), 12.5))
	cid := cidOf(t, dev.waitUsage(5*time.Second))
	if e.cmd.Pending("f412fa87c9e0") == 0 {
		t.Error("下行之后应当有待确认的指令")
	}
	dev.ack(cid, true, "")
	waitPendingZero(t, e, "f412fa87c9e0")
}

// 设备回 ok=0（刻意不应用）是永久失败，立即结案不再重投。
func TestEndToEndNackStopsRetry(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")
	dev, _ := pairAndReconnect(t, e, "f412fa87c9e0", devKey, code)

	cid := cidOf(t, dev.waitPair(5*time.Second))
	dev.ack(cid, false, "stale_key")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if o, ok := e.cmd.LastOutcome("f412fa87c9e0"); ok && !o.OK {
			if o.Err != "stale_key" {
				t.Errorf("err = %q，期望 stale_key", o.Err)
			}
			if n := e.cmd.Pending("f412fa87c9e0"); n != 0 {
				t.Errorf("ok=0 之后还挂着 %d 条", n)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("没等到 ok=0 的结局")
}

// 重复投递：同一 cid 确认两次，服务端不能乱。
// 设备侧的幂等（同一 cid 只应用一次）在固件里，这里验服务端这半边。
func TestEndToEndDuplicateAckIsHarmless(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")
	dev, _ := pairAndReconnect(t, e, "f412fa87c9e0", devKey, code)

	cid := cidOf(t, dev.waitPair(5*time.Second))
	dev.ack(cid, true, "")
	dev.ack(cid, true, "") // 重投撞上在途回执时会出现

	waitPendingZero(t, e, "f412fa87c9e0")
}

// pending 密钥不能替受害设备伪造回执——它的 dev 和受害设备完全一样，
// 而 tracker 只比对 dev，全靠 ACL 这一道。
func TestEndToEndPendingKeyCannotForgeAck(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")
	dev, _ := pairAndReconnect(t, e, "f412fa87c9e0", devKey, code)
	cid := cidOf(t, dev.waitPair(5*time.Second))

	// 知道 MAC 的人注册一把 pending 密钥，连上来想替它回执。
	// 用不同的 client id：同 id 会触发 MQTT 会话接管，把合法设备顶掉，
	// 那测的就是顶号而不是 ACL（顶号是另一个问题，已单独报）。
	pendingKey, _ := e.register("f412fa87c9e0")
	imp, err := e.tryConnectDeviceAs("f412fa87c9e0", pendingKey, "f412fa87c9e0-imp")
	if err != nil {
		t.Fatalf("pending 密钥连不上：%v", err)
	}
	if !dev.c.IsConnected() {
		t.Fatal("合法设备被顶掉了，这条测的就不是 ACL 了")
	}
	if err := imp.publishAck(cid, true, ""); err == nil {
		// paho 的 publish 不一定报错（ACL 拒绝时 broker 可能静默丢弃），
		// 真正的判据是服务端有没有被骗——下面那条。
		t.Log("pending 的 publish 没报错，继续看服务端是否被骗")
	}
	time.Sleep(500 * time.Millisecond)
	if n := e.cmd.Pending("f412fa87c9e0"); n != 1 {
		t.Fatalf("待确认 = %d，pending 密钥把受害设备的指令结案了", n)
	}
	// 真正的设备仍能正常确认。
	dev.ack(cid, true, "")
	waitPendingZero(t, e, "f412fa87c9e0")
}

func waitPendingZero(t *testing.T, e *env, dev string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.cmd.Pending(dev) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s 仍有 %d 条待确认", dev, e.cmd.Pending(dev))
}
