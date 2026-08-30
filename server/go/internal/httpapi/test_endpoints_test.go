package httpapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/device"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

// attachPrinter 让 registry 认为这台桥上插着某台打印机，并写好它的机型档案。
// 走的是心跳那条真实路径（KindHeartbeat 带 serial），不是直接改内部状态。
func attachPrinter(t *testing.T, dep *testDeps, dev, serial string) {
	t.Helper()
	if err := dep.store.UpsertPrinter(store.Printer{
		Serial: serial, VID: "03F0", PID: "F22A", Make: "HP",
		Model: "HP Laser MFP 136a", CMD: "URF,PCL,PJL",
		URFCaps: "CP1,IS1-4-5-19,MT1-2-3-4-5-6,RS600,V1.4,W8", LastDev: dev,
	}); err != nil {
		t.Fatal(err)
	}
	// 最小机型档案，好让 render-profile 能算出纸张尺寸。
	writeIdent(t, dep, dev,
		`{"usb":{"vid":"0x03F0","pid":"0xF22A","serial":"`+serial+`"},`+
			`"printer_class":{"device_id":"MFG:HP;CMD:URF,PCL;MDL:HP Laser MFP 136a;",`+
			`"urf":"CP1,IS1-4-5-19,MT1-2-3-4-5-6,RS600,V1.4,W8"}}`)
	dep.reg.Send(dev, device.Msg{Kind: device.KindHeartbeat, Serial: serial})
	// actor 是异步处理的，等它把 serial 吃进去。
	for i := 0; i < 200; i++ {
		if act := dep.reg.Actor(dev); act != nil && act.Serial() == serial {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("actor 没能在 1 秒内认到打印机 %s", serial)
}

func jobCount(t *testing.T, dep *testDeps, dev string) int {
	t.Helper()
	jobs, err := dep.store.JobsForDevice(dev, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(jobs)
}

// 开始一轮测试，队列里必须真的出现对应份数的作业。
//
// 这条是为一个真实的漏做写的：适配测试界面做完了，点「开始测试」什么都
// 没打印——因为 handleTestStart 只建了一条 test_run 记录，从没入队任何作业。
// 界面、端点、状态机全都「工作正常」，只是纸上什么都没有。
func TestStartTestEnqueuesPages(t *testing.T) {
	h, dep := newTestAPI(t)
	token := login(t, h, dep, "13800008888")
	post(h, "/api/device/enroll", `{"dev":"f412fa87c9e0","name":"x"}`, bearer(token))
	attachPrinter(t, dep, "f412fa87c9e0", "CNB1S5N3S7")

	before := jobCount(t, dep, "f412fa87c9e0")

	rr := post(h, "/api/device/f412fa87c9e0/tests/double_print/start", `{}`, bearer(token))
	if rr.Code != 200 {
		t.Fatalf("start = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		RunID      string `json:"run_id"`
		JobsNeeded int    `json:"jobs_needed"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out.JobsNeeded != 2 {
		t.Fatalf("jobs_needed = %d，期望 2", out.JobsNeeded)
	}

	after := jobCount(t, dep, "f412fa87c9e0")
	if after-before != out.JobsNeeded {
		t.Fatalf("入队了 %d 件，说好的 %d 件——用户点了会什么都不打印",
			after-before, out.JobsNeeded)
	}

	// 作业要带上 run_id，否则派发时那一轮的一次性 quirk 覆盖不会跟着下去，
	// 测的就是默认配置而不是变体。
	jobs, _ := dep.store.JobsForDevice("f412fa87c9e0", 10)
	tagged := 0
	for _, j := range jobs {
		if j.TestRun == out.RunID {
			tagged++
			if j.State != store.StateQueued {
				t.Errorf("测试作业状态 = %q，期望 queued", j.State)
			}
			if j.Serial != "CNB1S5N3S7" {
				t.Errorf("测试作业的 serial = %q，必须是当前插着的那台", j.Serial)
			}
		}
	}
	if tagged != out.JobsNeeded {
		t.Errorf("带 run_id 的作业只有 %d 件", tagged)
	}
}

// 没插打印机时不入队，也不留下开着的一轮——否则下次点会撞 409。
func TestStartTestWithoutPrinterLeavesNothingBehind(t *testing.T) {
	h, dep := newTestAPI(t)
	token := login(t, h, dep, "13800008888")
	post(h, "/api/device/enroll", `{"dev":"f412fa87c9e0","name":"x"}`, bearer(token))

	rr := post(h, "/api/device/f412fa87c9e0/tests/double_print/start", `{}`, bearer(token))
	if rr.Code != 409 {
		t.Fatalf("没插打印机时 start = %d，期望 409（设备和测试都在，只是做不了）", rr.Code)
	}
	if n := jobCount(t, dep, "f412fa87c9e0"); n != 0 {
		t.Errorf("不该入队任何作业，入了 %d 件", n)
	}
	if _, open, _ := dep.store.OpenTestRun("f412fa87c9e0"); open {
		t.Error("不该留下开着的一轮——下次点会撞 409 说「已有一轮在进行中」")
	}
}
