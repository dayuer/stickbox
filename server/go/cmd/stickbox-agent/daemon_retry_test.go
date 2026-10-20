package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingClaude 是会记录 `-p /usage` 调用次数的伪造 claude。
// usage.out / auth.out 可在两轮之间改写，模拟 claude 状态变化。
type countingClaude struct {
	dir string
	// Path 传给 agentConfig.ClaudePath。
	Path string
}

func newCountingClaude(t *testing.T, usageOut, authOut string) *countingClaude {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("伪造 claude 用 POSIX shell")
	}
	c := &countingClaude{dir: t.TempDir()}
	c.Path = filepath.Join(c.dir, "claude")
	script := "#!/bin/sh\n" +
		"d=$(dirname \"$0\")\n" +
		"if [ \"$1\" = auth ]; then cat \"$d/auth.out\"; exit 0; fi\n" +
		"echo x >> \"$d/usage.calls\"\n" +
		"cat \"$d/usage.out\"\n"
	if err := os.WriteFile(c.Path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c.SetUsage(usageOut)
	c.SetAuth(authOut)
	return c
}

func (c *countingClaude) write(name, body string) {
	if err := os.WriteFile(filepath.Join(c.dir, name), []byte(body), 0o644); err != nil {
		panic(err)
	}
}
func (c *countingClaude) SetUsage(s string) { c.write("usage.out", s) }
func (c *countingClaude) SetAuth(s string)  { c.write("auth.out", s) }

// UsageCalls 是 `-p /usage` 被拉起的次数。
func (c *countingClaude) UsageCalls() int {
	b, err := os.ReadFile(filepath.Join(c.dir, "usage.calls"))
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "x")
}

// e2e 把真实的 daemonRound（读配置 → 伪造 claude → httptest 服务端）接到
// runDaemonWith 上，睡眠与时钟全部注入。afterSleep 在每次睡眠后调用（可改伪造环境），
// 返回 false 结束循环。
type e2e struct {
	sleeps []time.Duration
	sent   []sentNote
	clock  time.Time
}

func runE2E(t *testing.T, claude *countingClaude, server string, rounds int, afterSleep func(round int)) *e2e {
	t.Helper()
	t.Setenv("STICKBOX_CONFIG_DIR", t.TempDir())
	if err := saveConfig(&agentConfig{Server: server, Token: "tok", ClaudePath: claude.Path}); err != nil {
		t.Fatal(err)
	}
	r := &e2e{clock: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
	logf := func(string, ...any) {}
	tracker := newFailureTracker(func(title, body string) error {
		r.sent = append(r.sent, sentNote{title, body})
		return nil
	}, logf)
	tracker.now = func() time.Time { return r.clock }

	n := 0
	var running int32
	base := withFailureNotify(daemonRound(logf), tracker)
	round := func() error {
		if atomic.AddInt32(&running, 1) != 1 {
			t.Error("两轮重叠了")
		}
		defer atomic.AddInt32(&running, -1)
		return base()
	}
	err := runDaemonWith(context.Background(), daemonConfig{
		logf:  logf,
		round: round,
		now:   func() time.Time { return r.clock },
		sleep: func(_ context.Context, d time.Duration) bool {
			r.sleeps = append(r.sleeps, d)
			r.clock = r.clock.Add(d)
			n++
			if n >= rounds {
				return false
			}
			if afterSleep != nil {
				afterSleep(n)
			}
			return true
		},
		onSchedule: func(fails int, next time.Time, cd bool) { recordSchedule(logf, fails, next, cd) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func okServer(hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":1,"devices":1}`))
	}))
}

// ① 上报超时 -> 60s 后重试成功 -> 回 10 分钟，状态清零。
func TestE2EUploadTimeoutRetriesAfterSixtySeconds(t *testing.T) {
	old := httpTimeout
	httpTimeout = 150 * time.Millisecond
	defer func() { httpTimeout = old }()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			select { // 第一次请求拖过客户端超时
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":1,"devices":1}`))
	}))
	defer srv.Close()

	claude := newCountingClaude(t, readFixture(t, "usage-stream.jsonl"), `{"loggedIn":true}`+"\n")
	var midState agentState
	r := runE2E(t, claude, srv.URL, 2, func(int) { midState = loadState() })

	want := []time.Duration{time.Minute, usageInterval}
	if !reflect.DeepEqual(r.sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", r.sleeps, want)
	}
	if hits != 2 || claude.UsageCalls() != 2 {
		t.Errorf("上报 %d 次、claude %d 次，期望各 2 次", hits, claude.UsageCalls())
	}
	if midState.ConsecutiveFailures != 1 || midState.NextRetryAt != r.clock.Add(-usageInterval).Unix() {
		t.Errorf("失败后的状态 = %+v", midState)
	}
	final := loadState()
	if final.ConsecutiveFailures != 0 || final.NextRetryAt != 0 || final.LastSuccessAt == 0 {
		t.Errorf("成功后状态 = %+v", final)
	}
	if len(r.sent) != 0 {
		t.Errorf("一次失败不该通知：%v", r.sent)
	}
}

// ② 服务端一直 503：60s、120s、240s 后回到 10 分钟；每轮只拉起一次 claude。
// 前 4 轮（7 分钟内）不通知，距第一次失败满 25 分钟（第 6 轮）才通知一次，之后不重复。
func TestE2EThreeTransientFailuresThenNormalInterval(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, `{"e":"unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	claude := newCountingClaude(t, readFixture(t, "usage-stream.jsonl"), `{"loggedIn":true}`+"\n")
	r := runE2E(t, claude, srv.URL, 7, nil)

	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, usageInterval, usageInterval, usageInterval, usageInterval}
	if !reflect.DeepEqual(r.sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", r.sleeps, want)
	}
	if claude.UsageCalls() != 7 || hits != 7 {
		t.Errorf("claude %d 次、上报 %d 次，期望各 7 次（串行，每轮一次）", claude.UsageCalls(), hits)
	}
	if len(r.sent) != 1 {
		t.Fatalf("同一原因一小时内只通知一次：%v", r.sent)
	}
	if s := loadState(); s.ConsecutiveFailures != 7 || s.NextRetryAt != r.clock.Unix() {
		t.Errorf("状态 = %+v", s)
	}
}

// ③ claude 未登录：不快速重试，保持 10 分钟，每轮只拉起一次 claude；
// 失败在 0/10/20/30 分钟，第 4 轮（距第一次失败满 25 分钟）通知一次。
func TestE2ENotLoggedInDoesNotFastRetry(t *testing.T) {
	var hits int32
	srv := okServer(&hits)
	defer srv.Close()

	claude := newCountingClaude(t, totalCostStream, `{"loggedIn":false}`+"\n")
	r := runE2E(t, claude, srv.URL, 4, nil)

	want := []time.Duration{usageInterval, usageInterval, usageInterval, usageInterval}
	if !reflect.DeepEqual(r.sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", r.sleeps, want)
	}
	if claude.UsageCalls() != 4 || hits != 0 {
		t.Errorf("claude %d 次、上报 %d 次，期望 4 次和 0 次", claude.UsageCalls(), hits)
	}
	s := loadState()
	if s.ClaudeAuth != stateAuthOut || s.ConsecutiveFailures != 4 {
		t.Errorf("状态 = %+v", s)
	}
	if len(r.sent) != 1 || !strings.Contains(r.sent[0].body, "claude auth login") {
		t.Errorf("第 4 轮应通知一次：%v", r.sent)
	}
}

// ④ limits 为 null 被跳过：算冷却，不快速重试；claude 恢复后成功。
func TestE2ELimitsNullRetriesThenRecovers(t *testing.T) {
	var hits int32
	srv := okServer(&hits)
	defer srv.Close()

	claude := newCountingClaude(t, readFixture(t, "usage-no-limits.jsonl"), `{"loggedIn":true}`+"\n")
	var midState agentState
	r := runE2E(t, claude, srv.URL, 2, func(int) {
		midState = loadState()
		claude.SetUsage(readFixture(t, "usage-stream.jsonl"))
	})

	want := []time.Duration{usageInterval, usageInterval}
	if !reflect.DeepEqual(r.sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", r.sleeps, want)
	}
	if hits != 1 {
		t.Errorf("跳过那轮不该上报，实际共 %d 次", hits)
	}
	if midState.ConsecutiveFailures != 1 || !strings.Contains(midState.LastError, "限额") {
		t.Errorf("跳过后的状态 = %+v", midState)
	}
	if s := loadState(); s.ConsecutiveFailures != 0 || s.LastSuccessAt == 0 {
		t.Errorf("恢复后的状态 = %+v", s)
	}
}

// ⑤ 令牌失效（401/403）：不快速重试，1 小时后再看。
func TestE2ETokenRejectedWaitsAnHour(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"e":"unauthorized"}`, code)
		}))
		claude := newCountingClaude(t, readFixture(t, "usage-stream.jsonl"), `{"loggedIn":true}`+"\n")
		r := runE2E(t, claude, srv.URL, 2, nil)
		srv.Close()
		want := []time.Duration{authRetryInterval, authRetryInterval}
		if !reflect.DeepEqual(r.sleeps, want) {
			t.Errorf("HTTP %d: sleeps = %v，期望 %v", code, r.sleeps, want)
		}
		if claude.UsageCalls() != 2 {
			t.Errorf("HTTP %d: claude %d 次，期望 2 次", code, claude.UsageCalls())
		}
	}
}

// ⑥ 假 claude 始终 rate_limits=null：间隔 10/20/40/60 分钟封顶，每轮只拉起一次 claude，
// 不上报；status 状态显示冷却中和下次尝试时间。
func TestE2ELimitsNullBacksOffToSixtyMinutes(t *testing.T) {
	var hits int32
	srv := okServer(&hits)
	defer srv.Close()

	claude := newCountingClaude(t, readFixture(t, "usage-no-limits.jsonl"), `{"loggedIn":true}`+"\n")
	r := runE2E(t, claude, srv.URL, 7, nil)

	m := time.Minute
	want := []time.Duration{10 * m, 20 * m, 40 * m, 60 * m, 60 * m, 60 * m, 60 * m}
	if !reflect.DeepEqual(r.sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", r.sleeps, want)
	}
	if claude.UsageCalls() != 7 || hits != 0 {
		t.Errorf("claude %d 次、上报 %d 次，期望 7 次和 0 次（每轮一次，串行）", claude.UsageCalls(), hits)
	}
	s := loadState()
	if !s.Cooldown || s.ConsecutiveFailures != 7 || s.NextRetryAt != r.clock.Unix() {
		t.Errorf("状态 = %+v", s)
	}
	if lines := s.statusLines(); !strings.Contains(lines[len(lines)-1], "冷却中") {
		t.Errorf("status 没有冷却行：%v", lines)
	}
}

// 通知阈值不变：失败发生在 0/10/30 分钟，第 3 轮（连续 3 轮且距上次成功/首次失败 ≥25 分钟）
// 才通知；之后一小时内同一原因不重复。
func TestE2ECooldownNotifiesAfterThresholdOnce(t *testing.T) {
	var hits int32
	srv := okServer(&hits)
	defer srv.Close()

	claude := newCountingClaude(t, readFixture(t, "usage-no-limits.jsonl"), `{"loggedIn":true}`+"\n")
	r := runE2E(t, claude, srv.URL, 4, nil) // 失败于 0/10/30/70 分钟；70-30 < 1 小时
	if len(r.sent) != 1 {
		t.Fatalf("应通知一次：%v", r.sent)
	}
}

// 冷却中恢复：回 10 分钟、状态清零；再次冷却从 10 分钟重新起算。
func TestE2ECooldownRecoversAndResets(t *testing.T) {
	var hits int32
	srv := okServer(&hits)
	defer srv.Close()

	claude := newCountingClaude(t, readFixture(t, "usage-no-limits.jsonl"), `{"loggedIn":true}`+"\n")
	r := runE2E(t, claude, srv.URL, 5, func(round int) {
		switch round {
		case 2: // 第 3 轮成功
			claude.SetUsage(readFixture(t, "usage-stream.jsonl"))
		case 3: // 第 4 轮又变回 null
			claude.SetUsage(readFixture(t, "usage-no-limits.jsonl"))
		}
	})
	m := time.Minute
	want := []time.Duration{10 * m, 20 * m, usageInterval, 10 * m, 20 * m}
	if !reflect.DeepEqual(r.sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", r.sleeps, want)
	}
	if hits != 1 {
		t.Errorf("只有恢复那一轮上报，实际 %d 次", hits)
	}
}

// 服务端回 429：同样归 cooldown，10 分钟起逐步退避，不走 60s 快速重试。
func TestE2EServer429BacksOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"e":"rate_limited"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	claude := newCountingClaude(t, readFixture(t, "usage-stream.jsonl"), `{"loggedIn":true}`+"\n")
	r := runE2E(t, claude, srv.URL, 3, nil)
	m := time.Minute
	if want := []time.Duration{10 * m, 20 * m, 40 * m}; !reflect.DeepEqual(r.sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", r.sleeps, want)
	}
	if !loadState().Cooldown {
		t.Errorf("状态应为冷却中：%+v", loadState())
	}
}
