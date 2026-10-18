package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/admin"
	"github.com/dayuer/stickbox/server/go/internal/broker"
	"github.com/dayuer/stickbox/server/go/internal/command"
	"github.com/dayuer/stickbox/server/go/internal/config"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

func TestParseDeviceShowArgs(t *testing.T) {
	dev, last, err := parseDeviceShowArgs([]string{"f412fa87c9e0", "--last", "7"})
	if err != nil || dev != "f412fa87c9e0" || last != 7 {
		t.Fatalf("dev=%q last=%d err=%v", dev, last, err)
	}
	if _, _, err := parseDeviceShowArgs([]string{"bad"}); err == nil {
		t.Error("非法 dev 应报错")
	}
	if _, _, err := parseDeviceShowArgs([]string{"f412fa87c9e0", "--last", "0"}); err == nil {
		t.Error("--last 0 应报错")
	}
	if _, _, err := parseDeviceShowArgs([]string{"f412fa87c9e0", "--bad"}); err == nil {
		t.Error("未知参数应报错")
	}
}

func TestFormatDeviceShow(t *testing.T) {
	reset := time.Unix(1792244076, 0)
	rep := admin.Report{
		Dev: "f412fa87c9e0", Bound: true, UserID: "u1",
		Keys: []admin.KeyInfo{
			{KeyID: "key-active", State: "active", Role: "device", UserID: "u1", Created: 1792240000},
			{KeyID: "key-pending", State: "pending", Role: "device", Created: 1792240100},
			{KeyID: "key-old", State: "disabled", Role: "device", Created: 1792230000},
		},
		ConnectedKeys: []string{"key-active"},
		RecentAuth: []broker.AuthEvent{
			{At: time.Unix(1792241000, 0), Dev: "f412fa87c9e0", Code: 5, Result: "invalid_key", Remote: "192.0.2.1:1"},
			{At: time.Unix(1792240900, 0), Dev: "f412fa87c9e0", Code: 0, Result: "ok", Remote: "192.0.2.1:2"},
		},
		Pending: []command.PendingInfo{
			{CID: "cid-1", Topic: "printer/f412fa87c9e0/usage", Tries: 2, NextAt: time.Unix(1792241100, 0)},
		},
		RegisterQuota: admin.QuotaInfo{Remaining: 3, Limit: 5, ResetAt: &reset},
		Usage: &admin.UsageInfo{
			FiveHour: &usage.Window{UsedPercentage: 37.5}, SevenDay: &usage.Window{UsedPercentage: 19},
			TZ: "Asia/Bangkok", ReceivedAt: time.Unix(1792241200, 0),
		},
		Frame: &admin.FrameInfo{Version: 12, Available: true},
		Photo: &admin.PhotoInfo{
			Cap: true, Ready: true, State: "ok", PLV: 7, Have: 12,
			Bad: []string{"a1b2c3d4"}, FreeKB: 1024 * 1024, InitDone: "abcdef01",
		},
	}
	out := formatDeviceShow(rep)
	for _, want := range []string{
		"设备 f412fa87c9e0", "用户 u1", "key-active", "active", "pending", "disabled",
		"当前连接：key-active", "0x05", "invalid_key", "cid-1", "tries=2",
		"五小时 37.5%", "七天 19.0%", "Asia/Bangkok", "帧版本：v12", "剩余 3/5",
		"照片：cap=true ready=true state=ok plv=7", "已用初始化 token abcdef01",
		"跳过：a1b2c3d4",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "hash") {
		t.Errorf("输出不应包含 hash：\n%s", out)
	}
}

func TestFetchDeviceReport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/device" || r.URL.Query().Get("dev") != "f412fa87c9e0" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(admin.Report{
			Dev: "f412fa87c9e0", Bound: true, UserID: "u1",
			Keys: []admin.KeyInfo{{KeyID: "k1", State: "active"}},
		})
	}))
	defer srv.Close()

	cfg := &config.Config{AdminAddr: strings.TrimPrefix(srv.URL, "http://")}
	rep, err := fetchDeviceReport(cfg, "f412fa87c9e0", 5)
	if err != nil {
		t.Fatal(err)
	}
	if rep.UserID != "u1" || len(rep.Keys) != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestFetchDeviceReportUnavailable(t *testing.T) {
	cfg := &config.Config{AdminAddr: "127.0.0.1:1"}
	if _, err := fetchDeviceReport(cfg, "f412fa87c9e0", 5); err == nil {
		t.Fatal("连不上时应报错")
	}
}
