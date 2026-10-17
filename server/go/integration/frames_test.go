package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/frame"
	"github.com/dayuer/stickbox/server/go/internal/frames"
	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

// tinyBuilder 让这个测试只验证「启动巡检会补帧」，不把真实渲染器的
// 耗时（-race 下更慢，且同一 store 上还有别的服务在后台渲染）算进
// 测试预算。渲染器本身由 frame/render 的测试和 T13c 集成测试覆盖。
type tinyBuilder struct{}

func (tinyBuilder) Build(source.Snapshot) (*frame.Bundle, error) {
	base := []byte("base")
	atlas := []byte("atlas")
	b := &frame.Bundle{Base: base, Atlas: atlas}
	b.Meta.V = frame.Version
	bs := sha256.Sum256(base)
	as := sha256.Sum256(atlas)
	b.Meta.Frame = hex.EncodeToString(bs[:])
	b.Meta.Atlas.SHA256 = hex.EncodeToString(as[:])
	b.Meta.Atlas.Size = len(atlas)
	return b, nil
}

type usageNoopPub struct{}

func (usageNoopPub) PublishUsage(string, []byte) error { return nil }

type frameRecPub struct {
	mu   sync.Mutex
	sent map[string][]uint64
}

func (p *frameRecPub) PublishFrame(dev string, payload []byte) error {
	var v struct {
		Ver uint64 `json:"ver"`
	}
	if json.Unmarshal(payload, &v) != nil || v.Ver == 0 {
		return nil
	}
	p.mu.Lock()
	if p.sent == nil {
		p.sent = map[string][]uint64{}
	}
	p.sent[dev] = append(p.sent[dev], v.Ver)
	p.mu.Unlock()
	return nil
}

func (p *frameRecPub) last(dev string) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	vs := p.sent[dev]
	if len(vs) == 0 {
		return 0, false
	}
	return vs[len(vs)-1], true
}

// 新版本部署/进程重启：库里已有已配对设备和用量、但 frames 目录是空的，
// 服务启动后必须自己补帧并发出 frame 通知，不能等下一次 agent 上报。
func TestFramesBackfillOnStartup(t *testing.T) {
	e := startServer(t)
	_, code := e.register("f412fa87c9e0")
	agentTok, dev, userID := e.pair(code, "")
	e.reportUsage(agentTok, usageReport(time.Now(), 15))

	pub := &frameRecPub{}
	fs, err := frames.New(
		t.TempDir(), // 全新的 frames 目录，模拟刚部署
		e.store,
		source.NewClaude(usage.NewService(e.store, usageNoopPub{}, time.Now), "UTC"),
		pub,
		tinyBuilder{},
		frames.Options{
			Debounce:          50 * time.Millisecond,
			RenderConcurrency: 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fs.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fs.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ver, ok := fs.Current(userID); ok && ver >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("启动后没有为已有用户生成帧")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		if ver, ok := pub.last(dev); ok && ver >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("启动补帧没有发出 frame 通知")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
