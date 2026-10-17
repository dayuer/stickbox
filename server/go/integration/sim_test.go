package integration

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/frame"
	"github.com/dayuer/stickbox/server/go/internal/sim"
)

// 模拟器走完整链路：注册 → MQTT（pending 只订 pair）→ agent 配对 →
// 服务端踢连接 → 自动重连 → 订阅 usage/frame → 收到帧通知 → 拉帧 →
// 按 FRAME.md 逐行拼接。结果必须与 frame.Bundle.Splice 逐字节一致。
func TestSimulatorRegisterPairFrameSplice(t *testing.T) {
	e := startServer(t)
	d, err := sim.New(sim.Config{
		Server:     e.httpBase,
		MQTTAddr:   e.mqttAddr,
		StateDir:   t.TempDir(),
		Insecure:   true,
		StreamAddr: "", // 本测试不起推流
		Logf:       t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	code, err := d.Register(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = d.Run(ctx) }()

	agentTok, _, _ := e.pair(code, "")

	waitFrame := func(what string) {
		t.Helper()
		select {
		case <-d.FrameReady():
		case <-time.After(15 * time.Second):
			t.Fatalf("%s：模拟器没有收到帧包", what)
		}
	}
	waitFrame("配对后")

	b := d.Bundle()
	if b == nil {
		t.Fatal("模拟器没有帧包")
	}
	for _, hm := range [][2]int{{0, 0}, {9, 5}, {12, 47}, {23, 59}} {
		got, err := sim.Splice(b, hm[0], hm[1])
		if err != nil {
			t.Fatalf("Splice(%02d:%02d): %v", hm[0], hm[1], err)
		}
		serverB := &frame.Bundle{Base: b.Base, Atlas: b.Atlas, Meta: b.Meta}
		want, err := serverB.Splice(hm[0], hm[1])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%02d:%02d 拼接与服务端不一致：%d vs %d 字节",
				hm[0], hm[1], len(got), len(want))
		}
	}

	// 清掉可能残留的信号，再上报用量，等新帧。
	for {
		select {
		case <-d.FrameReady():
			continue
		default:
		}
		break
	}
	e.reportUsage(agentTok, usageReport(time.Now(), 33))
	waitFrame("上报用量后")
	if d.Bundle() == b {
		t.Error("上报用量后帧包没有更新")
	}
}
