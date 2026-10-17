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

// 完整用量链路：agent 上报 → 服务端落库 → MQTT printer/{dev}/usage → 设备。
//
// 单测都过不代表串起来能用——mochi 的 Serve() 非阻塞那个 bug 就是
// 只有真跑起来才暴露的。
func TestEndToEndUsageDelivery(t *testing.T) {
	const phone, code = "13800000000", "424242"
	e := startServerWithDevLogin(t, phone, code)
	appTok := e.loginWithCode(phone, code)
	devKey := e.enroll(appTok, "f412fa87c9e0")
	dev := e.connectDevice("f412fa87c9e0", devKey)
	dev.heartbeat("ready")

	if err := dev.subscribeUsage(); err != nil {
		t.Fatalf("订阅用量 = %v", err)
	}
	e.sendCode(phone)
	agentTok := e.agentLoginWithCode(phone, code)

	before := time.Now().Unix()
	e.reportUsage(agentTok, usageReport(time.Now(), 12.5))
	got := parseUsage(t, dev.waitUsage(5*time.Second))
	after := time.Now().Unix()

	if got.FiveHour == nil || got.FiveHour.UsedPercentage != 12.5 {
		t.Errorf("five_hour = %+v，期望 12.5", got.FiveHour)
	}
	if got.SevenDay == nil || got.SevenDay.UsedPercentage != 6.25 {
		t.Errorf("seven_day = %+v，期望 6.25", got.SevenDay)
	}
	if got.Now < before || got.Now > after {
		t.Errorf("now = %d，不在 [%d,%d]——设备要靠它和服务端对表", got.Now, before, after)
	}
	if got.TS < before || got.TS > after {
		t.Errorf("ts = %d，不在 [%d,%d]", got.TS, before, after)
	}
}

// 设备重连并重新订阅时，应立即收到补发。
//
// usage 没有 retain（payload 的 now 会过期），所以补发必须由 OnSubscribed
// 现场生成一份新 now；ts 仍是服务端最初收到数据的时间。
func TestEndToEndUsageResendOnReconnect(t *testing.T) {
	const phone, code = "13800000000", "424242"
	e := startServerWithDevLogin(t, phone, code)
	appTok := e.loginWithCode(phone, code)
	devKey := e.enroll(appTok, "f412fa87c9e0")

	first := e.connectDevice("f412fa87c9e0", devKey)
	first.heartbeat("ready")
	if err := first.subscribeUsage(); err != nil {
		t.Fatalf("首次订阅 = %v", err)
	}
	first.disconnect()

	e.sendCode(phone)
	agentTok := e.agentLoginWithCode(phone, code)
	before := time.Now().Unix()
	e.reportUsage(agentTok, usageReport(time.Now(), 33))
	after := time.Now().Unix()

	// 隔开一秒，确保补发的 now 一定大于最初收到的 ts。
	time.Sleep(1200 * time.Millisecond)

	second := e.connectDevice("f412fa87c9e0", devKey)
	second.heartbeat("ready")
	if err := second.subscribeUsage(); err != nil {
		t.Fatalf("重连订阅 = %v", err)
	}
	got := parseUsage(t, second.waitUsage(5*time.Second))

	if got.TS < before || got.TS > after {
		t.Errorf("ts = %d，期望是服务端最初收到的时间 [%d,%d]", got.TS, before, after)
	}
	if got.Now <= got.TS {
		t.Errorf("now = %d 不晚于 ts = %d——补发必须现场盖 now", got.Now, got.TS)
	}
	if got.FiveHour == nil || got.FiveHour.UsedPercentage != 33 {
		t.Errorf("five_hour = %+v，期望 33", got.FiveHour)
	}
}

// ACL：设备不能订阅别人的 topic。
func TestEndToEndACLBlocksCrossDevice(t *testing.T) {
	e := startServer(t)
	token := e.login("13800008888")
	devKey := e.enroll(token, "f412fa87c9e0")
	dev := e.connectDevice("f412fa87c9e0", devKey)

	if err := dev.subscribe("printer/aaaaaaaaaaaa/usage"); err == nil {
		t.Error("设备订阅了别人的 topic——ACL 没生效")
	}
}

// 认证失败必须被拒，不能是「连上了但收不到消息」那种半死状态。
func TestEndToEndRejectsBadKey(t *testing.T) {
	e := startServer(t)
	if _, err := e.tryConnectDevice("f412fa87c9e0", "bogus.key"); err == nil {
		t.Error("错误密钥竟然连上了")
	}
}

// device 角色的 username 必须等于密钥里的 dev，防止拿 A 的密钥冒充 B。
func TestEndToEndRejectsMismatchedUsername(t *testing.T) {
	e := startServer(t)
	token := e.login("13800008888")
	devKey := e.enroll(token, "f412fa87c9e0")
	if _, err := e.tryConnectDevice("aaaaaaaaaaaa", devKey); err == nil {
		t.Error("用别的 username 拿着 A 的密钥连上了")
	}
}

// agent 令牌只能走 HTTP，不能连 MQTT。
func TestEndToEndAgentCannotConnectMQTT(t *testing.T) {
	const phone, code = "13800000000", "424242"
	e := startServerWithDevLogin(t, phone, code)
	e.loginWithCode(phone, code)
	e.sendCode(phone)
	agentTok := e.agentLoginWithCode(phone, code)
	if _, err := e.tryConnectDevice("f412fa87c9e0", agentTok); err == nil {
		t.Error("agent 令牌连上了 MQTT")
	}
}

// 固定手机号：不发短信也能登录，测试链路不依赖短信服务商。
func TestEndToEndDevLogin(t *testing.T) {
	e := startServerWithDevLogin(t, "13800000000", "424242")
	token := e.loginWithCode("13800000000", "424242")
	if token == "" {
		t.Fatal("固定手机号登录失败")
	}
	if e.sender.last != "" {
		t.Errorf("固定号码发了真短信：%q", e.sender.last)
	}
	// 连续登录不该被 60 秒闸挡住
	if e.loginWithCode("13800000000", "424242") == "" {
		t.Error("连续登录被限流了，测试没法用")
	}
}
