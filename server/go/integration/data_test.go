package integration

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

func dataReport(date string, gmv, gmvGoal, orders, ordersGoal float64) string {
	return fmt.Sprintf(`{"date":"%s","tz":"UTC",`+
		`"gmv":{"value":%g,"goal":%g},`+
		`"orders":{"value":%g,"goal":%g},`+
		`"card_revenue":{"value":8000,"goal":10000}}`,
		date, gmv, gmvGoal, orders, ordersGoal)
}

// POST /api/data 的完整链路：落库 → 标脏 → 重渲 → MQTT 通知 → 设备取到新帧。
//
// 并且验证「哪一路上屏跟着最近一次上报走」：先报额度看到额度看板，再报经营
// 数据看板就换过去，再报额度又换回来。换源靠的是帧内容真的变了，不是版本号
// 自己往前走——所以这里比对字节。
func TestEndToEndDataDistribution(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")
	agentTok, _, _ := e.pair(code, "")

	dev := e.connectDevice("f412fa87c9e0", devKey)
	if err := dev.subscribeFrame(); err != nil {
		t.Fatalf("订阅 frame = %v", err)
	}
	verPair := dev.waitFrameVer(10 * time.Second)

	// 1) 先报 Claude 额度：消耗型看板。
	e.reportUsage(agentTok, usageReport(time.Now(), 21.5))
	verUsage := dev.waitFrameVer(10 * time.Second)
	if verUsage <= verPair {
		t.Fatalf("上报用量后版本没前进：%d → %d", verPair, verUsage)
	}
	st, frameUsage, _ := e.getFrame(devKey, "")
	if st != 200 {
		t.Fatalf("取额度看板帧 = %d %s", st, frameUsage)
	}

	// 2) 再报经营数据（更新）：看板要换成达成型。
	today := time.Now().UTC().Format("2006-01-02")
	e.reportData(agentTok, dataReport(today, 146_000, 200_000, 420, 400))
	verData := dev.waitFrameVer(10 * time.Second)
	if verData <= verUsage {
		t.Fatalf("上报经营数据后版本没前进：%d → %d", verUsage, verData)
	}
	st, frameData, etag := e.getFrame(devKey, "")
	if st != 200 {
		t.Fatalf("取经营看板帧 = %d %s", st, frameData)
	}
	if want := fmt.Sprintf(`"quota-%d"`, verData); etag != want {
		t.Errorf("ETag = %q，期望 %q", etag, want)
	}
	if string(frameData[:4]) != "SBF1" {
		t.Fatalf("帧包头部不对：%.4s", frameData)
	}
	if bytes.Equal(frameUsage, frameData) {
		t.Error("报了经营数据但帧内容没变：看板没有切过去")
	}

	// 3) 又报额度（最新）：切回消耗型看板。
	e.reportUsage(agentTok, usageReport(time.Now(), 44))
	verBack := dev.waitFrameVer(10 * time.Second)
	if verBack <= verData {
		t.Fatalf("再报用量后版本没前进：%d → %d", verData, verBack)
	}
	st, frameBack, _ := e.getFrame(devKey, "")
	if st != 200 {
		t.Fatalf("取回切后的帧 = %d %s", st, frameBack)
	}
	if bytes.Equal(frameBack, frameData) {
		t.Error("最近一次上报是用量，看板应当切回额度口径")
	}
}

// 跨零点：昨天的数据不能继续挂成「今日」。达成率归零，但帧照样出得来。
func TestDataStaleDayStillRenders(t *testing.T) {
	e := startServer(t)
	devKey, code := e.register("f412fa87c9e0")
	agentTok, _, _ := e.pair(code, "")

	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	e.reportData(agentTok, dataReport(yesterday, 198_000, 200_000, 390, 400))

	deadline := time.Now().Add(10 * time.Second)
	for {
		st, body, _ := e.getFrame(devKey, "")
		if st == 200 && len(body) > 16 && string(body[:4]) == "SBF1" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("隔天数据没能渲出帧：st=%d len=%d", st, len(body))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
