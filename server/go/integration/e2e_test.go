package integration

import (
	"encoding/json"
	"testing"
	"time"
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
// 设备收到 paired=true → agent 上报用量 → 设备收到。
func TestEndToEndRegisterPairUsage(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")

	dev := e.connectDevice("f412fa87c9e0", devKey)
	dev.heartbeat()
	if err := dev.subscribeUsage(); err != nil {
		t.Fatalf("订阅用量 = %v", err)
	}
	if err := dev.subscribePair(); err != nil {
		t.Fatalf("订阅配对 = %v", err)
	}

	agentTok, pairedDev, _ := e.pair(code, "")
	if pairedDev != "f412fa87c9e0" {
		t.Fatalf("配对的 dev = %q", pairedDev)
	}
	if !pairPayload(t, dev.waitPair(5*time.Second)) {
		t.Error("配对通知不是 paired=true")
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
