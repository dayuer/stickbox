package integration

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/frame"
	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/theme"
)

// recBuilder 记录每次渲染用的快照，同时复用 tinyBuilder 产出合法帧包。
type recBuilder struct {
	tinyBuilder
	mu    sync.Mutex
	last  source.Snapshot
	calls int
}

func (b *recBuilder) Build(th theme.Theme, s source.Snapshot) (*frame.Bundle, error) {
	b.mu.Lock()
	b.last = s
	b.calls++
	b.mu.Unlock()
	return b.tinyBuilder.Build(th, s)
}

func (b *recBuilder) snapshot() source.Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

// T29 集成：设置用户时区 → 立即重渲并通知设备换帧；清除 → 回退 agent 上报。
func TestUserTZSetAndClearRerendersImmediately(t *testing.T) {
	b := &recBuilder{}
	e, _ := startServerAtWith(t, t.TempDir(), b)

	devKey, code := e.register("f412fa87c9e0")
	agentTok, _, _ := e.pair(code, "")

	// agent 上报时带 Asia/Tokyo。
	rep := map[string]any{
		"five_hour": map[string]any{
			"used_percentage": 10,
			"resets_at":       time.Now().Add(time.Hour).Unix(),
		},
		"tz": "Asia/Tokyo",
	}
	raw, _ := json.Marshal(rep)
	e.reportUsage(agentTok, raw)

	d := e.connectDevice("f412fa87c9e0", devKey)
	if err := d.subscribeFrame(); err != nil {
		t.Fatalf("订阅 frame = %v", err)
	}
	ver1 := d.waitFrameVer(10 * time.Second)
	if loc := b.snapshot().Now.Location().String(); loc != "Asia/Tokyo" {
		t.Fatalf("初始时区 = %s，期望 Asia/Tokyo", loc)
	}

	// 设置用户时区：应立刻出下一个版本，并推送到设备。
	st, body := e.req("PUT", "/api/user/tz", `{"tz":"Pacific/Kiritimati"}`,
		map[string]string{"Content-Type": "application/json",
			"Authorization": "Bearer " + agentTok})
	if st != 200 {
		t.Fatalf("PUT tz = %d %s", st, body)
	}
	ver2 := d.waitFrameVer(10 * time.Second)
	if ver2 <= ver1 {
		t.Fatalf("设置时区后没有换帧：%d → %d", ver1, ver2)
	}
	if loc := b.snapshot().Now.Location().String(); loc != "Pacific/Kiritimati" {
		t.Fatalf("渲染时区 = %s，期望 Pacific/Kiritimati", loc)
	}
	st, _, etag := e.getFrame(devKey, "")
	if st != 200 {
		t.Fatalf("GET frame = %d", st)
	}
	if etag != quoted(ver2) {
		t.Fatalf("ETag = %s，期望 %s", etag, quoted(ver2))
	}

	// GET 返回生效值与来源。
	st, body = e.req("GET", "/api/user/tz", "",
		map[string]string{"Authorization": "Bearer " + agentTok})
	if st != 200 {
		t.Fatalf("GET tz = %d %s", st, body)
	}
	var got struct {
		TZ     string `json:"tz"`
		Source string `json:"source"`
	}
	json.Unmarshal(body, &got)
	if got.TZ != "Pacific/Kiritimati" || got.Source != "user" {
		t.Fatalf("GET tz = %+v", got)
	}

	// 清除：回退到 agent 上报的 Asia/Tokyo，再换一版。
	st, body = e.req("PUT", "/api/user/tz", `{"tz":""}`,
		map[string]string{"Content-Type": "application/json",
			"Authorization": "Bearer " + agentTok})
	if st != 200 {
		t.Fatalf("清除 tz = %d %s", st, body)
	}
	ver3 := d.waitFrameVer(10 * time.Second)
	if ver3 <= ver2 {
		t.Fatalf("清除后没有换帧：%d → %d", ver2, ver3)
	}
	if loc := b.snapshot().Now.Location().String(); loc != "Asia/Tokyo" {
		t.Fatalf("清除后时区 = %s，期望回退 Asia/Tokyo", loc)
	}
}

// 不带 theme 参数（老固件）时 ETag 是纯数字，见 frames.Service.ETag。
func quoted(v uint64) string { return fmt.Sprintf(`"%d"`, v) }
