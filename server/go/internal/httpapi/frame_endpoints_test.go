package httpapi

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/frame"
	"github.com/dayuer/stickbox/server/go/internal/frames"
	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

type fakeFrameUsage struct {
	mu    sync.Mutex
	pcts  map[string]float64
	noUse bool
}

func (f *fakeFrameUsage) Latest(userID string) (usage.Report, time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noUse {
		return usage.Report{}, time.Time{}, false, nil
	}
	if f.pcts == nil {
		f.pcts = map[string]float64{}
	}
	p, ok := f.pcts[userID]
	if !ok {
		p = float64(10 + 10*len(f.pcts))
		f.pcts[userID] = p
	}
	return usage.Report{
		FiveHour: &usage.Window{
			UsedPercentage: p,
			ResetsAt:       time.Now().Add(time.Hour).Unix(),
		},
	}, time.Now(), true, nil
}

type fakeFramePub struct{}

func (fakeFramePub) PublishFrame(string, []byte) error { return nil }

type fakeFrameBuilder struct {
	mu    sync.Mutex
	calls int
	fail  error
}

func (b *fakeFrameBuilder) Build(s source.Snapshot) (*frame.Bundle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.fail != nil {
		return nil, b.fail
	}
	p := "none"
	if len(s.Windows) > 0 {
		p = fmt.Sprintf("%.0f", s.Windows[0].UsedPercentage*100)
	}
	return frameTestBundle(p), nil
}

func (b *fakeFrameBuilder) setFail(err error) {
	b.mu.Lock()
	b.fail = err
	b.mu.Unlock()
}

func frameTestBundle(payload string) *frame.Bundle {
	base := []byte("base:" + payload)
	atlas := []byte("atlas:" + payload)
	b := &frame.Bundle{Base: base, Atlas: atlas}
	b.Meta.V = frame.Version
	bs := sha256.Sum256(base)
	as := sha256.Sum256(atlas)
	b.Meta.Frame = hex.EncodeToString(bs[:])
	b.Meta.Atlas.SHA256 = hex.EncodeToString(as[:])
	b.Meta.Atlas.Size = len(atlas)
	return b
}

func newFrameAPI(t *testing.T) (http.Handler, *testDeps, *frames.Service, *fakeFrameBuilder) {
	t.Helper()
	h, dep := newTestAPI(t)
	b := &fakeFrameBuilder{}
	fs, err := frames.New(dep.cfg.Root, dep.store, &fakeFrameUsage{}, fakeFramePub{}, b, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	dep.api.SetFrames(fs)
	return h, dep, fs, b
}

func frameMeta(t *testing.T, body []byte) *frame.Meta {
	t.Helper()
	if len(body) < 16 {
		t.Fatalf("帧包太短：%d", len(body))
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
	return meta
}

func TestDeviceFrameETagAnd304(t *testing.T) {
	h, _, _, _ := newFrameAPI(t)
	devTok, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")

	rr := get(h, "/api/device/frame", bearer(devTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("取帧 = %d %s", rr.Code, rr.Body)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	etag := rr.Header().Get("ETag")
	if etag != `"1"` {
		t.Errorf("ETag = %q，期望 \"1\"", etag)
	}
	if ver := rr.Header().Get("X-Frame-Version"); ver != "1" {
		t.Errorf("X-Frame-Version = %q", ver)
	}
	if meta := frameMeta(t, rr.Body.Bytes()); meta.V != frame.Version {
		t.Errorf("meta.V = %d", meta.V)
	}

	// If-None-Match 命中：304 且没有 body。
	hdr := bearer(devTok)
	hdr["If-None-Match"] = etag
	rr2 := get(h, "/api/device/frame", hdr)
	if rr2.Code != http.StatusNotModified || rr2.Body.Len() != 0 {
		t.Errorf("304 = %d，body %d 字节", rr2.Code, rr2.Body.Len())
	}

	// 上报用量后版本加一，旧 ETag 不再命中。
	body := fmt.Sprintf(`{"five_hour":{"used_percentage":5,"resets_at":%d}}`,
		time.Now().Add(time.Hour).Unix())
	if rr := post(h, "/api/usage", body, bearer(agentTok)); rr.Code != 200 {
		t.Fatalf("上报 = %d %s", rr.Code, rr.Body)
	}
	rr3 := get(h, "/api/device/frame", bearer(devTok))
	if rr3.Code != 200 || rr3.Header().Get("ETag") != `"2"` {
		t.Errorf("上报后取帧 = %d ETag=%q", rr3.Code, rr3.Header().Get("ETag"))
	}
	hdr["If-None-Match"] = etag
	if rr := get(h, "/api/device/frame", hdr); rr.Code != 200 {
		t.Errorf("旧 ETag = %d，期望 200", rr.Code)
	}
}

// 解绑后也要重建：剩余设备会收到新版本。
func TestUnpairRebuildsFrame(t *testing.T) {
	h, _, fs, _ := newFrameAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, dev, userID := mustPair(t, h, code, "")

	if _, ver, ok := fs.Current(userID); !ok || ver != 1 {
		t.Fatalf("配对后帧 = ver %d ok=%v，期望 ver 1", ver, ok)
	}
	rr := post(h, "/api/agent/unpair", `{"dev":"`+dev+`"}`, bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("解绑 = %d %s", rr.Code, rr.Body)
	}
	if _, ver, ok := fs.Current(userID); !ok || ver != 2 {
		t.Errorf("解绑后帧 = ver %d ok=%v，期望 ver 2", ver, ok)
	}
}

func TestDeviceFrameUnpairedForbidden(t *testing.T) {
	h, _, _, _ := newFrameAPI(t)
	devTok, _ := register(t, h, "f412fa87c9e0")
	if rr := get(h, "/api/device/frame", bearer(devTok)); rr.Code != http.StatusForbidden {
		t.Errorf("未配对取帧 = %d %s，期望 403", rr.Code, rr.Body)
	}
}

func TestDeviceFrameNoFrame(t *testing.T) {
	h, _, _, b := newFrameAPI(t)
	b.setFail(errors.New("render failed"))
	devTok, code := register(t, h, "f412fa87c9e0")
	mustPair(t, h, code, "")
	if rr := get(h, "/api/device/frame", bearer(devTok)); rr.Code != http.StatusNotFound {
		t.Errorf("没有帧时 = %d %s，期望 404", rr.Code, rr.Body)
	}
}

// 两台屏属于不同用户时，各自拿到自己的帧，不会串。
func TestDeviceFrameIsPerUser(t *testing.T) {
	h, _, _, _ := newFrameAPI(t)
	devA, codeA := register(t, h, "f412fa87c9e0")
	_, _, userA := mustPair(t, h, codeA, "")
	devB, codeB := register(t, h, "aabbccddee01")
	_, _, userB := mustPair(t, h, codeB, "")

	ra := get(h, "/api/device/frame", bearer(devA))
	rb := get(h, "/api/device/frame", bearer(devB))
	if ra.Code != 200 || rb.Code != 200 {
		t.Fatalf("取帧 A=%d B=%d", ra.Code, rb.Code)
	}
	a, err := frames.Unpack(ra.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	b, err := frames.Unpack(rb.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Base) == string(b.Base) {
		t.Fatalf("两个用户拿到同一帧：%q", a.Base)
	}
	// fakeFrameUsage 按首次出现顺序分配 10%、20%。
	if userA == userB {
		t.Fatal("两个用户 ID 相同")
	}
	if string(a.Base) != "base:10" || string(b.Base) != "base:20" {
		t.Errorf("A=%q B=%q，期望 base:10 / base:20", a.Base, b.Base)
	}
}
