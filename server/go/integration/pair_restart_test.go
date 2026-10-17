package integration

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

// 服务端重启后 broker 的 retained 消息丢失。已配对设备重连、订阅 pair
// 时必须按自己这把密钥的状态收到 paired=true，然后才能订阅 usage/frame。
func TestPairStateResentAfterServerRestart(t *testing.T) {
	const dev = "f412fa87c9e0"
	dir := t.TempDir()

	e1, stop1 := startServerAt(t, dir)
	devKey, code := e1.register(dev)
	agentTok, pairedDev, _ := e1.pair(code, "")
	if pairedDev != dev {
		t.Fatalf("配对的 dev = %q", pairedDev)
	}
	keyID, _, err := auth.SplitToken(devKey)
	if err != nil {
		t.Fatal(err)
	}
	stop1() // broker 里的 retained pair=true 随进程一起没了

	e2, _ := startServerAt(t, dir)
	d := e2.connectDevice(dev, devKey)
	if err := d.subscribePair(); err != nil {
		t.Fatalf("订阅 pair：%v", err)
	}

	var note struct {
		Paired bool   `json:"paired"`
		KeyID  string `json:"key_id"`
	}
	if err := json.Unmarshal(d.waitPair(3*time.Second), &note); err != nil {
		t.Fatalf("配对通知解析失败：%v", err)
	}
	if !note.Paired || note.KeyID != keyID {
		t.Fatalf("配对通知 = %+v，期望 paired=true key_id=%s", note, keyID)
	}

	// 拿到 paired=true 之后设备才会订阅数据主题，两步都必须能过。
	if err := d.subscribeUsage(); err != nil {
		t.Fatalf("订阅 usage：%v", err)
	}
	if err := d.subscribeFrame(); err != nil {
		t.Fatalf("订阅 frame：%v", err)
	}
	e2.reportUsage(agentTok, usageReport(time.Now(), 27))
	got := parseUsage(t, d.waitUsage(5*time.Second))
	if got.FiveHour == nil || got.FiveHour.UsedPercentage != 27 {
		t.Errorf("重启后设备收到 = %+v", got.FiveHour)
	}
}
