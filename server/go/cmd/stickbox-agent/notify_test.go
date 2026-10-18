package main

import (
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
