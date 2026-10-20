package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want outcome
	}{
		{"ok", nil, outcomeOK},
		{"skip limits null", &skipError{reason: "rate_limits.limits 为 null"}, outcomeCooldown},
		{"5xx", fmt.Errorf("上报失败：%w", &apiError{Status: 502}), outcomeTransient},
		{"500", fmt.Errorf("上报失败：%w", &apiError{Status: 500}), outcomeTransient},
		{"401", fmt.Errorf("上报失败：%w", &apiError{Status: 401, Code: "unauthorized"}), outcomePersistent},
		{"403", fmt.Errorf("上报失败：%w", &apiError{Status: 403}), outcomePersistent},
		{"400", fmt.Errorf("上报失败：%w", &apiError{Status: 400}), outcomePersistent},
		{"429", fmt.Errorf("上报失败：%w", &apiError{Status: 429}), outcomeCooldown},
		{"transport", fmt.Errorf("上报失败：%w", &transportError{msg: "连接失败", err: errors.New("i/o timeout")}), outcomeTransient},
		{"not paired", errNotPaired, outcomePersistent},
		{"not logged in", errClaudeNotLoggedIn, outcomePersistent},
		{"no report", &noReportError{}, outcomePersistent},
		{"parse", fmt.Errorf("解析 rate_limits.limits 失败：%w", errors.New("bad")), outcomePersistent},
		{"unknown", errors.New("boom"), outcomePersistent},
	}
	for _, c := range cases {
		if got := classify(c.err); got != c.want {
			t.Errorf("%s: classify = %s，期望 %s", c.name, got, c.want)
		}
	}
}

// 真实的 HTTP 超时 / 连接被拒 / 500 经过 apiClient.call 后分别归类正确。
func TestClassifyRealHTTPFailures(t *testing.T) {
	// 5xx
	srv5 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"e":"boom"}`, http.StatusBadGateway)
	}))
	defer srv5.Close()
	if _, err := postUsage(&agentConfig{Server: srv5.URL, Token: "t"}, usagePayload{}); classify(err) != outcomeTransient {
		t.Errorf("502 应为 transient：%v", err)
	}

	// 401
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"e":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv4.Close()
	if _, err := postUsage(&agentConfig{Server: srv4.URL, Token: "t"}, usagePayload{}); classify(err) != outcomePersistent {
		t.Errorf("401 应为 persistent：%v", err)
	}

	// 连接被拒：关掉服务端后再请求
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	_, err := postUsage(&agentConfig{Server: url, Token: "t"}, usagePayload{})
	if classify(err) != outcomeTransient {
		t.Errorf("连接被拒应为 transient：%v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "连接") {
		t.Errorf("错误文本应保持原样：%v", err)
	}
}

// 5xx 以外的 TLS 证书错误不会自己好，不该快速重试。
func TestClassifyTLSCertIsPersistent(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := postUsage(&agentConfig{Server: srv.URL, Token: "t"}, usagePayload{})
	if err == nil || classify(err) != outcomePersistent {
		t.Errorf("自签证书应为 persistent：%v", err)
	}
}

func TestFastRetryScheduleIsSixtyOneTwentyTwoForty(t *testing.T) {
	want := []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second}
	if len(fastRetryDelays) != len(want) {
		t.Fatalf("fastRetryDelays = %v", fastRetryDelays)
	}
	for i := range want {
		if fastRetryDelays[i] != want[i] {
			t.Errorf("第 %d 次 = %s，期望 %s", i+1, fastRetryDelays[i], want[i])
		}
	}
}

type sched struct {
	fails int
	next  time.Time
}

var daemonT0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

// driveDaemon 按脚本依次返回每轮结果，记录每次睡眠时长；脚本用完后让 sleep 返回
// false 退出。假时钟随睡眠前进，并检查轮与轮之间没有重叠。
func driveDaemon(t *testing.T, script []error) (sleeps []time.Duration, scheds []sched) {
	t.Helper()
	clock := daemonT0
	var running int32
	i := 0
	round := func() error {
		if atomic.AddInt32(&running, 1) != 1 {
			t.Error("两轮重叠了")
		}
		defer atomic.AddInt32(&running, -1)
		err := script[i]
		i++
		return err
	}
	sleep := func(_ context.Context, d time.Duration) bool {
		if atomic.LoadInt32(&running) != 0 {
			t.Error("睡眠时仍有一轮在跑")
		}
		sleeps = append(sleeps, d)
		clock = clock.Add(d)
		return i < len(script)
	}
	err := runDaemonWith(context.Background(), daemonConfig{
		logf:  func(string, ...any) {},
		round: round, sleep: sleep,
		now: func() time.Time { return clock },
		onSchedule: func(fails int, next time.Time) {
			scheds = append(scheds, sched{fails, next})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return sleeps, scheds
}

var (
	errTransient    = &transportError{msg: "连接失败：i/o timeout", err: errors.New("timeout")}
	errTransient5xx = fmt.Errorf("上报失败：%w", &apiError{Status: 503})
	errSkipped      = &skipError{reason: "rate_limits.limits 为 null"}
	errPersistent   = errClaudeNotLoggedIn
)

// 一次超时后 60s 重试，成功后回 10 分钟。
func TestDaemonRetriesSixtySecondsAfterTimeoutThenRecovers(t *testing.T) {
	sleeps, scheds := driveDaemon(t, []error{errTransient, nil})
	want := []time.Duration{time.Minute, usageInterval}
	if !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", sleeps, want)
	}
	if scheds[0].fails != 1 || scheds[1].fails != 0 {
		t.Errorf("连续失败计数 = %+v", scheds)
	}
	if got := scheds[0].next.Sub(daemonT0); got != time.Minute {
		t.Errorf("next_retry 应为 now+60s，实际 +%s", got)
	}
}

// 连续 3 次 transient：60s、120s、240s，之后回到 10 分钟，并且不再开新一串快速重试。
func TestDaemonThreeTransientsThenBackToNormalInterval(t *testing.T) {
	script := []error{errTransient, errTransient5xx, errTransient, errTransient, errTransient5xx, nil}
	sleeps, scheds := driveDaemon(t, script)
	want := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, // 3 次快速重试
		usageInterval, usageInterval, // 之后回正常间隔
		usageInterval, // 成功
	}
	if !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", sleeps, want)
	}
	for i, wantFails := range []int{1, 2, 3, 4, 5, 0} {
		if scheds[i].fails != wantFails {
			t.Errorf("第 %d 轮后连续失败 = %d，期望 %d", i+1, scheds[i].fails, wantFails)
		}
	}
}

// 成功后再次失败，重新从 60s 开始。
func TestDaemonFastRetryRestartsAfterSuccess(t *testing.T) {
	sleeps, _ := driveDaemon(t, []error{errTransient, nil, errTransient, errTransient, nil})
	want := []time.Duration{time.Minute, usageInterval, time.Minute, 2 * time.Minute, usageInterval}
	if !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", sleeps, want)
	}
}

// 未登录等 persistent 不快速重试；未配对/401/403 仍是 1 小时。
func TestDaemonPersistentFailuresKeepExistingIntervals(t *testing.T) {
	sleeps, scheds := driveDaemon(t, []error{
		errPersistent,
		errors.New("claude 输出里没有 usage_report"),
		errNotPaired,
		fmt.Errorf("上报失败：%w", &apiError{Status: 401}),
		fmt.Errorf("上报失败：%w", &apiError{Status: 403}),
		nil,
	})
	want := []time.Duration{usageInterval, usageInterval, authRetryInterval, authRetryInterval, authRetryInterval, usageInterval}
	if !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", sleeps, want)
	}
	// 未配对不计入连续失败（与通知计数一致）：1、2、2、3、4、0
	for i, wantFails := range []int{1, 2, 2, 3, 4, 0} {
		if scheds[i].fails != wantFails {
			t.Errorf("第 %d 轮后连续失败 = %d，期望 %d", i+1, scheds[i].fails, wantFails)
		}
	}
}

// 持续性故障夹在中间会重置快速重试串。
func TestDaemonPersistentResetsFastRetryBudget(t *testing.T) {
	sleeps, _ := driveDaemon(t, []error{errTransient, errPersistent, errTransient})
	want := []time.Duration{time.Minute, usageInterval, time.Minute}
	if !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", sleeps, want)
	}
}

// ① 连续 null：10、20、40、60 分钟，之后封顶 60。
func TestDaemonCooldownBacksOffTenTwentyFortySixty(t *testing.T) {
	sleeps, scheds := driveDaemon(t, []error{errSkipped, errSkipped, errSkipped, errSkipped, errSkipped, errSkipped})
	m := time.Minute
	want := []time.Duration{10 * m, 20 * m, 40 * m, 60 * m, 60 * m, 60 * m}
	if !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", sleeps, want)
	}
	for i := range scheds {
		if scheds[i].fails != i+1 {
			t.Errorf("第 %d 轮后连续失败 = %d", i+1, scheds[i].fails)
		}
	}
}

// 429 与 null 同属 cooldown，共用一串退避。
func TestDaemonCooldown429SharesBackoff(t *testing.T) {
	e429 := fmt.Errorf("上报失败：%w", &apiError{Status: 429})
	sleeps, _ := driveDaemon(t, []error{errSkipped, e429, errSkipped})
	m := time.Minute
	if want := []time.Duration{10 * m, 20 * m, 40 * m}; !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", sleeps, want)
	}
}

// ② 成功后复位：回 10 分钟，下一次冷却又从 10 分钟起。
func TestDaemonCooldownResetsAfterSuccess(t *testing.T) {
	sleeps, _ := driveDaemon(t, []error{errSkipped, errSkipped, nil, errSkipped})
	m := time.Minute
	if want := []time.Duration{10 * m, 20 * m, usageInterval, 10 * m}; !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", sleeps, want)
	}
}

// 冷却串里夹一次网络错误：该次走 60s 快速重试，不打断冷却的退避级别。
func TestDaemonTransientInsideCooldownKeepsLevel(t *testing.T) {
	sleeps, _ := driveDaemon(t, []error{errSkipped, errTransient, errSkipped})
	m := time.Minute
	if want := []time.Duration{10 * m, m, 20 * m}; !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", sleeps, want)
	}
}

// 冷却期内未登录（persistent）清掉冷却串。
func TestDaemonPersistentClearsCooldown(t *testing.T) {
	sleeps, _ := driveDaemon(t, []error{errSkipped, errSkipped, errPersistent, errSkipped})
	m := time.Minute
	if want := []time.Duration{10 * m, 20 * m, usageInterval, 10 * m}; !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", sleeps, want)
	}
}

// 抖动：±20%，随机源可注入；边界值精确。
func TestCooldownDelayJitter(t *testing.T) {
	m := time.Minute
	cases := []struct {
		n    int
		r    float64
		want time.Duration
	}{
		{1, 0, 8 * m}, {1, 0.5, 10 * m}, {1, 1, 12 * m},
		{2, 0, 16 * m}, {3, 0.5, 40 * m}, {4, 0, 48 * m}, {4, 1, 72 * m},
		{9, 0.5, 60 * m}, {9, 0, 48 * m},
	}
	for _, c := range cases {
		r := c.r
		got := cooldownDelay(c.n, func() float64 { return r })
		if got != c.want {
			t.Errorf("n=%d r=%v: %s，期望 %s", c.n, c.r, got, c.want)
		}
	}
	if got := cooldownDelay(2, nil); got != 20*m {
		t.Errorf("nil 随机源应不抖动：%s", got)
	}
}

// runDaemon 把 rand 传给 planner。
func TestDaemonUsesInjectedJitter(t *testing.T) {
	clock := daemonT0
	var sleeps []time.Duration
	err := runDaemonWith(context.Background(), daemonConfig{
		logf:  func(string, ...any) {},
		round: func() error { return errSkipped },
		now:   func() time.Time { return clock },
		rand:  func() float64 { return 0 },
		sleep: func(_ context.Context, d time.Duration) bool { sleeps = append(sleeps, d); return len(sleeps) < 2 },
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{8 * time.Minute, 16 * time.Minute}; !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v，期望 %v", sleeps, want)
	}
}
