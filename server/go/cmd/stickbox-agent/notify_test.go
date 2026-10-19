package main

import (
	"context"
	"errors"
	"reflect"
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

// newSilentTracker：上次成功在 notifyAfterSilence 之前，时间条件已满足，
// 只剩「连续 3 轮」这个条件，用来单独测轮数与节流。
func newSilentTracker() (*failureTracker, *[]sentNote, *time.Time) {
	tr, sent, now := newTestTracker()
	tr.record(nil)
	*now = now.Add(notifyAfterSilence)
	return tr, sent, now
}

// 驱动真实的快速重试节奏，返回每轮失败后的累计时间与通知数。
func driveNotify(t *testing.T, lastOKBefore bool, rounds int) (sleeps []time.Duration, sentAt []time.Duration, sent *[]sentNote) {
	tr, sent, now := newTestTracker()
	start := *now
	if lastOKBefore {
		tr.record(nil) // 起点刚成功过一次
	}
	n := 0
	round := func() error { n++; return errTransient }
	err := runDaemonWith(context.Background(), daemonConfig{
		logf: func(string, ...any) {}, round: withFailureNotify(round, tr),
		sleep: func(_ context.Context, d time.Duration) bool {
			sleeps = append(sleeps, d)
			// 记录本轮失败结束时（睡眠前）已通知的数量变化发生的时间点
			if len(sentAt) < len(*sent) {
				sentAt = append(sentAt, now.Sub(start))
			}
			*now = now.Add(d)
			return n < rounds
		},
		now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return sleeps, sentAt, sent
}

// 快速重试把轮数很快推到 3，但通知还要等「距上次成功 >= 25 分钟」：
// 一次几分钟的抖动（60s+120s+240s 重试期间）不弹通知。
func TestFastRetriesDoNotShortenNotifyWindow(t *testing.T) {
	// 4 轮：失败发生在 0、1m、3m、7m（相对第一次失败），远小于 25 分钟。
	sleeps, _, sent := driveNotify(t, true, 4)
	if len(sleeps) != 4 || sleeps[0] != time.Minute || sleeps[1] != 2*time.Minute || sleeps[2] != 4*time.Minute {
		t.Fatalf("sleeps = %v", sleeps)
	}
	if len(*sent) != 0 {
		t.Fatalf("7 分钟内的抖动不该通知：%v", *sent)
	}
}

// 故障持续：失败在 0、1、3、7、17、27 分钟。距上次成功 >= 25 分钟时才通知，且只通知一次。
func TestSustainedFailureNotifiesOnceAfterWindow(t *testing.T) {
	sleeps, sentAt, sent := driveNotify(t, true, 6)
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, usageInterval, usageInterval, usageInterval}
	if !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v", sleeps)
	}
	if len(*sent) != 1 {
		t.Fatalf("应只通知一次：%v", *sent)
	}
	// 第 6 轮失败发生在 27 分钟（>= 25），在随后的睡眠前被记录。
	if len(sentAt) != 1 || sentAt[0] < notifyAfterSilence {
		t.Fatalf("通知时间 %v 早于 %s", sentAt, notifyAfterSilence)
	}
	if !strings.Contains((*sent)[0].body, "6") {
		t.Errorf("通知应写明连续轮数：%q", (*sent)[0].body)
	}
}

func TestFailureTrackerNotifiesAtThirdFailure(t *testing.T) {
	tr, sent, _ := newSilentTracker()
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
	tr, sent, now := newSilentTracker()
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

// 轮数够了但时间不够（距上次成功不足 25 分钟）不通知；时间一到下一次失败就通知。
func TestFailureTrackerNeedsBothCountAndTime(t *testing.T) {
	tr, sent, now := newTestTracker()
	e := errors.New("boom")
	tr.record(nil)
	for i := 0; i < 5; i++ {
		*now = now.Add(4 * time.Minute) // 5 轮共 20 分钟
		tr.record(e)
	}
	if len(*sent) != 0 {
		t.Fatalf("距上次成功 20 分钟不该通知：%v", *sent)
	}
	*now = now.Add(5 * time.Minute) // 25 分钟
	tr.record(e)
	if len(*sent) != 1 {
		t.Fatalf("满 25 分钟应通知：%v", *sent)
	}
}

// 进程里没见过成功时，窗口从第一次失败算起。
func TestFailureTrackerWindowStartsAtFirstFailureWithoutSuccess(t *testing.T) {
	tr, sent, now := newTestTracker()
	e := errors.New("boom")
	for i := 0; i < 3; i++ {
		tr.record(e)
	}
	if len(*sent) != 0 {
		t.Fatalf("刚开始失败不该通知：%v", *sent)
	}
	*now = now.Add(notifyAfterSilence)
	tr.record(e)
	if len(*sent) != 1 {
		t.Fatalf("满窗口应通知：%v", *sent)
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
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	tr.now = func() time.Time { return now }
	tr.record(nil)
	now = now.Add(notifyAfterSilence)
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
