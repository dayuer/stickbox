package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b"), []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}
	du := snapshotDir(dir)
	if du.Err != "" || du.Bytes != 8 || du.Files != 2 {
		t.Fatalf("du = %+v", du)
	}
	if du := snapshotDir(filepath.Join(dir, "missing")); du.Err == "" {
		t.Fatal("不存在的目录应返回错误")
	}
}

func TestParseResult(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{}}
{"type":"result","total_cost_usd":0,"num_turns":1,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":300,"cache_creation_input_tokens":4}}
`
	got := parseResult([]byte(stream))
	if got.TotalCostUSD != 0 || got.NumTurns != 1 {
		t.Fatalf("result = %+v", got)
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 2 ||
		got.Usage.CacheReadInputTokens != 300 || got.Usage.CacheCreationInputTokens != 4 {
		t.Fatalf("usage = %+v", got.Usage)
	}
	if parseResult([]byte("not json\n")).Type != "" {
		t.Fatal("非 JSON 行应被忽略")
	}
}

func TestParseLoadavg(t *testing.T) {
	for _, in := range []string{
		"{ 1.23 1.11 1.05 }\n",
		"1.23 1.11 1.05 2/345 67890\n",
	} {
		a, b, c, err := parseLoadavg(in)
		if err != nil || a != 1.23 || b != 1.11 || c != 1.05 {
			t.Fatalf("%q → %v %v %v %v", in, a, b, c, err)
		}
	}
	if _, _, _, err := parseLoadavg("garbage"); err == nil {
		t.Fatal("非法输入应报错")
	}
}

func TestSummarize(t *testing.T) {
	at := time.Now()
	all := []sample{
		{N: 1, At: at, DurationMS: 300, OK: true, RateLimitsOK: true, Load1: 2, PeakRSSMB: 100},
		{N: 2, At: at, DurationMS: 100, Skip: "冷却", Load1: 4, PeakRSSMB: 120},
		{N: 3, At: at, DurationMS: 200, Err: "超时", ExitCode: -1, Load1: 3, PeakRSSMB: 110},
	}
	sum := summarize(all, at)
	if sum.Runs != 3 || sum.OK != 1 || sum.Skipped != 1 || sum.Errors != 1 || sum.RateLimitsOK != 1 {
		t.Fatalf("计数 = %+v", sum)
	}
	if sum.DurationMS.Min != 100 || sum.DurationMS.Avg != 200 ||
		sum.DurationMS.P50 != 200 || sum.DurationMS.P95 != 300 || sum.DurationMS.Max != 300 {
		t.Fatalf("时长 = %+v", sum.DurationMS)
	}
	if sum.PeakRSSMB != 120 || sum.Load1Max != 4 || sum.Load1Avg != 3 {
		t.Fatalf("内存/负载 = %+v", sum)
	}
	if sum.PeakRSSStat.Min != 100 || sum.PeakRSSStat.Avg != 110 ||
		sum.PeakRSSStat.P50 != 110 || sum.PeakRSSStat.P95 != 120 ||
		sum.PeakRSSStat.Max != 120 {
		t.Fatalf("RSS 分布 = %+v", sum.PeakRSSStat)
	}
	if sum.FirstErr != "超时" {
		t.Fatalf("FirstErr = %q", sum.FirstErr)
	}
}
