package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type sentNote struct{ title, body string }

func newTestTracker() (*failureTracker, *[]sentNote, *time.Time) {
	var sent []sentNote
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	tr := newFailureTracker(func(title, body string) error {
		sent = append(sent, sentNote{title, body})
		return nil
	}, func(string, ...any) {})
	tr.now = func() time.Time { return now }
	return tr, &sent, &now
}

// 快速重试的每一轮都算「连续失败」：60s、120s 后的第 3 轮失败就达到阈值；
// 同一原因之后一小时内（含后续 10 分钟间隔的轮次）不再重复通知。
func TestFastRetriesCountTowardNotifyAndStillThrottled(t *testing.T) {
	tr, sent, now := newTestTracker()
	script := []error{errTransient, errTransient, errTransient, errTransient, errTransient, errTransient}
	i := 0
	round := func() error { e := script[i]; i++; return e }
	var sleeps []time.Duration
	sleep := func(_ context.Context, d time.Duration) bool {
		sleeps = append(sleeps, d)
		*now = now.Add(d)
		return i < len(script)
	}
	err := runDaemonWith(context.Background(), daemonConfig{
		logf: func(string, ...any) {}, round: withFailureNotify(round, tr),
		sleep: sleep, now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	// 6 轮：60s、120s、240s、10m、10m、10m，总时间约 33 分钟，都在 1 小时内。
	if len(sleeps) != 6 || sleeps[2] != 4*time.Minute || sleeps[3] != usageInterval {
		t.Fatalf("sleeps = %v", sleeps)
	}
	if len(*sent) != 1 {
		t.Fatalf("同一原因一小时内只通知一次，实际 %d 次：%v", len(*sent), *sent)
	}
	if !strings.Contains((*sent)[0].body, "3") {
		t.Errorf("第 3 轮就该通知：%q", (*sent)[0].body)
	}
}

func TestFailureTrackerNotifiesAtThirdFailure(t *testing.T) {
	tr, sent, _ := newTestTracker()
	e := errors.New("boom")
	tr.record(e)
	tr.record(e)
	if len(*sent) != 0 {
		t.Fatalf("前两轮不该通知：%v", *sent)
	}
	tr.record(e)
	if len(*sent) != 1 || !strings.Contains((*sent)[0].body, "boom") || !strings.Contains((*sent)[0].body, "3") {
		t.Fatalf("第三轮应通知一次：%v", *sent)
	}
}

func TestFailureTrackerRateLimitsPerReason(t *testing.T) {
	tr, sent, now := newTestTracker()
	a, b := errors.New("A"), errors.New("B")
	for i := 0; i < 5; i++ {
		tr.record(a)
	}
	if len(*sent) != 1 {
		t.Fatalf("同一原因一小时内只通知一次：%d", len(*sent))
	}
	tr.record(b) // 另一个原因，立即可通知
	if len(*sent) != 2 {
		t.Fatalf("新原因应通知：%d", len(*sent))
	}
	*now = now.Add(59 * time.Minute)
	tr.record(a)
	if len(*sent) != 2 {
		t.Fatalf("59 分钟内不该重复：%d", len(*sent))
	}
	*now = now.Add(2 * time.Minute)
	tr.record(a)
	if len(*sent) != 3 {
		t.Fatalf("过了一小时应再通知：%d", len(*sent))
	}
}

func TestFailureTrackerSuccessResets(t *testing.T) {
	tr, sent, _ := newTestTracker()
	e := errors.New("boom")
	tr.record(e)
	tr.record(e)
	tr.record(nil)
	tr.record(e)
	tr.record(e)
	if len(*sent) != 0 {
		t.Fatalf("成功后计数应清零：%v", *sent)
	}
}

func TestFailureTrackerIgnoresNotPaired(t *testing.T) {
	tr, sent, _ := newTestTracker()
	for i := 0; i < 5; i++ {
		tr.record(errNotPaired)
	}
	if len(*sent) != 0 {
		t.Fatalf("未配对不算故障：%v", *sent)
	}
}

func TestFailureTrackerNotifyErrorOnlyLogged(t *testing.T) {
	var logged []string
	tr := newFailureTracker(func(string, string) error { return errors.New("没有 notify-send") },
		func(f string, a ...any) { logged = append(logged, f) })
	for i := 0; i < 3; i++ {
		tr.record(errors.New("x"))
	}
	if len(logged) != 1 {
		t.Fatalf("通知失败应记一条日志：%v", logged)
	}
}

func TestSystemNotifyOS(t *testing.T) {
	var gotName string
	var gotArgs []string
	run := func(name string, args ...string) error { gotName, gotArgs = name, args; return nil }
	look := func(n string) (string, error) { return "/usr/bin/" + n, nil }

	if err := systemNotifyOS("darwin", look, run, `T"1`, "行1\n行2 \\ \"q\""); err != nil {
		t.Fatal(err)
	}
	if gotName != "osascript" || len(gotArgs) != 2 || gotArgs[0] != "-e" {
		t.Fatalf("darwin 应调 osascript -e：%s %v", gotName, gotArgs)
	}
	want := `display notification "行1 行2 \\ \"q\"" with title "T\"1"`
	if gotArgs[1] != want {
		t.Errorf("脚本 = %s\n期望 %s", gotArgs[1], want)
	}

	if err := systemNotifyOS("linux", look, run, "T", "B"); err != nil {
		t.Fatal(err)
	}
	if gotName != "/usr/bin/notify-send" || gotArgs[0] != "T" || gotArgs[1] != "B" {
		t.Errorf("linux 应调 notify-send：%s %v", gotName, gotArgs)
	}

	noTool := func(string) (string, error) { return "", errors.New("not found") }
	if err := systemNotifyOS("linux", noTool, run, "T", "B"); err == nil {
		t.Error("缺 notify-send 应返回错误（由上层记日志）")
	}
	if err := systemNotifyOS("windows", look, run, "T", "B"); err == nil {
		t.Error("windows 只记日志，应返回错误")
	}
}
