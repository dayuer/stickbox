// claudeusage-harness 是本机 L-E0 实验工具：每 5 分钟串行跑一次
// `claude -p /usage`，记录耗时、峰值内存、退出码、rate_limits 是否非空、
// 费用/turn/token 和系统负载。日志只写 /tmp，不入库。
//
//	go build ./cmd/claudeusage-harness
//	caffeinate -dimsu ./claudeusage-harness --runs 3 --interval 15s   # 短验证
//	caffeinate -dimsu ./claudeusage-harness --duration 24h            # 长跑
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/claudeusage"
)

type tokens struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

type sample struct {
	N             int       `json:"n"`
	At            time.Time `json:"at"`
	DurationMS    int64     `json:"duration_ms"`
	OK            bool      `json:"ok"`
	Skip          string    `json:"skip,omitempty"`
	Err           string    `json:"err,omitempty"`
	ExitCode      int       `json:"exit_code"`
	RateLimitsOK  bool      `json:"rate_limits_ok"`
	FiveHourOK    bool      `json:"five_hour_ok"`
	SevenDayOK    bool      `json:"seven_day_ok"`
	FiveHourPct   *float64  `json:"five_hour_pct,omitempty"`
	SevenDayPct   *float64  `json:"seven_day_pct,omitempty"`
	CostUSD       float64   `json:"cost_usd"`
	NumTurns      int       `json:"num_turns"`
	Tokens        tokens    `json:"tokens"`
	PeakRSSMB     float64   `json:"peak_rss_mb"`
	Load1         float64   `json:"load1"`
	Load5         float64   `json:"load5"`
	Load15        float64   `json:"load15"`
	LoadErr       string    `json:"load_err,omitempty"`
	StdoutBytes   int       `json:"stdout_bytes"`
	ProjectsBytes int64     `json:"projects_bytes"`
	ProjectsFiles int64     `json:"projects_files"`
	ProjectsErr   string    `json:"projects_err,omitempty"`
}

type resultEvent struct {
	Type         string  `json:"type"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
	Usage        struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func parseResult(out []byte) resultEvent {
	var last resultEvent
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev resultEvent
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type == "result" {
			last = ev
		}
	}
	return last
}

// parseLoadavg 解析 `{ 1.23 1.11 1.05 }` 或 `1.23 1.11 1.05 2/345 67890`。
func parseLoadavg(s string) (float64, float64, float64, error) {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(s), "{}"))
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("loadavg 输出异常：%q", s)
	}
	a, e1 := strconv.ParseFloat(fields[0], 64)
	b, e2 := strconv.ParseFloat(fields[1], 64)
	c, e3 := strconv.ParseFloat(fields[2], 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, 0, fmt.Errorf("解析 loadavg 失败：%q", s)
	}
	return a, b, c, nil
}

// underCaffeinate 检查父进程是不是 caffeinate，用来拦住没包 caffeinate 的长跑。
func underCaffeinate() bool {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(os.Getppid())).Output()
	return err == nil && strings.Contains(strings.ToLower(string(out)), "caffeinate")
}

// loadAvg 读系统 1/5/15 分钟负载。失败返回 error，调用方原样记录。
func loadAvg() (float64, float64, float64, error) {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			return 0, 0, 0, err
		}
		return parseLoadavg(string(out))
	}
	out, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0, err
	}
	return parseLoadavg(string(out))
}

// diskUsage 是 ~/.claude/projects 的体量快照。
type diskUsage struct {
	Bytes int64  `json:"bytes"`
	Files int64  `json:"files"`
	Err   string `json:"err,omitempty"`
}

// snapshotProjects 统计会话 JSONL 所在目录的总字节数和文件数。
// 只读，不跟随符号链接。
func snapshotProjects() diskUsage {
	home, err := os.UserHomeDir()
	if err != nil {
		return diskUsage{Err: err.Error()}
	}
	root := filepath.Join(home, ".claude", "projects")
	var du diskUsage
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单个文件读不到不影响整体
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		du.Bytes += info.Size()
		du.Files++
		return nil
	})
	if err != nil {
		du.Err = err.Error()
	}
	return du
}

func main() {
	var (
		interval = flag.Duration("interval", 5*time.Minute, "两次采样的间隔")
		duration = flag.Duration("duration", 24*time.Hour, "总时长；0 = 直到 Ctrl-C")
		runs     = flag.Int("runs", 0, "只跑 N 次（>0 时覆盖 duration）")
		timeout  = flag.Duration("timeout", 90*time.Second, "单次 claude 超时")
		outPath  = flag.String("out", "", "JSONL 输出路径（默认 /tmp/claudeusage-<时间>.jsonl）")
		cfgDir   = flag.String("config-dir", "", "专用工作目录的父目录（默认用户配置目录/stickbox）")
		claude   = flag.String("claude", "", "claude 可执行文件（默认 PATH）")
		noCaff   = flag.Bool("allow-no-caffeinate", false, "长跑时允许不经 caffeinate（默认拒绝）")
	)
	flag.Parse()

	if *outPath == "" {
		*outPath = filepath.Join(os.TempDir(),
			"claudeusage-"+time.Now().Format("20060102-150405")+".jsonl")
	}
	claudePath := *claude
	if claudePath == "" {
		var err error
		claudePath, err = exec.LookPath("claude")
		if err != nil {
			fatalf("PATH 里找不到 claude：%v", err)
		}
	}
	baseDir := *cfgDir
	if baseDir == "" {
		userCfg, err := os.UserConfigDir()
		if err != nil {
			fatalf("找不到用户配置目录：%v", err)
		}
		baseDir = filepath.Join(userCfg, "stickbox")
	}
	work, err := claudeusage.WorkDir(baseDir)
	if err != nil {
		fatalf("%v", err)
	}

	f, err := os.OpenFile(*outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fatalf("打开输出失败：%v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("claudeusage harness\n  claude: %s\n  work: %s\n  out: %s\n"+
		"  interval=%s duration=%s runs=%d timeout=%s\n",
		claudePath, work, *outPath, *interval, *duration, *runs, *timeout)
	longRun := *runs == 0 && *duration >= time.Hour
	if runtime.GOOS == "darwin" {
		if !longRun {
			fmt.Println("caffeinate 检查：短跑，不强制")
		} else if !*noCaff && !underCaffeinate() {
			fatalf("长跑必须用 caffeinate 包住：caffeinate -dimsu %s ...", os.Args[0])
		} else {
			fmt.Println("caffeinate 检查：通过（长跑期间防休眠）")
		}
	}

	started := time.Now()
	projStart := snapshotProjects()
	all := make([]sample, 0, 8)
	ask := ""
	consecutiveEmpty := 0
	for n := 1; ; n++ {
		if ctx.Err() != nil {
			break
		}
		runStart := time.Now()
		s := runOnce(ctx, n, claudePath, work, *timeout)
		all = append(all, s)
		if err := enc.Encode(s); err != nil {
			fatalf("写日志失败：%v", err)
		}
		fmt.Printf("[%d] %s ok=%v skip=%q err=%q dur=%.1fs rss=%.1fMB cost=%.4f turns=%d load=%.2f\n",
			n, s.At.Format("15:04:05"), s.OK, s.Skip, s.Err,
			float64(s.DurationMS)/1000, s.PeakRSSMB, s.CostUSD, s.NumTurns, s.Load1)

		if s.CostUSD > 0 && ask == "" {
			ask = fmt.Sprintf("total_cost_usd=%.6f > 0", s.CostUSD)
		}
		if s.RateLimitsOK {
			consecutiveEmpty = 0
		} else {
			consecutiveEmpty++
			if consecutiveEmpty >= 3 && ask == "" {
				ask = "连续 3 次 rate_limits 为空"
			}
		}
		if ask != "" {
			fmt.Printf("ASK: %s（停止采样，等待 master 决定）\n", ask)
			break
		}
		last := (*runs > 0 && n >= *runs) ||
			(*duration > 0 && time.Since(started) >= *duration)
		if last {
			break
		}

		wait := *interval - time.Since(runStart)
		if wait > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		} else {
			fmt.Printf("  本次耗时 %s 超过间隔 %s，立即开始下一次（仍串行）\n",
				time.Since(runStart), *interval)
		}
	}

	summary := summarize(all, started)
	summary.OutPath = *outPath
	summary.Ask = ask
	summary.ProjectsStart = projStart
	summary.ProjectsEnd = snapshotProjects()
	summary.ProjectsDeltaBytes = summary.ProjectsEnd.Bytes - summary.ProjectsStart.Bytes
	summaryPath := strings.TrimSuffix(*outPath, ".jsonl") + ".summary.json"
	if b, err := json.MarshalIndent(summary, "", "  "); err == nil {
		_ = os.WriteFile(summaryPath, append(b, '\n'), 0o600)
	}
	fmt.Printf("\n汇总（%s）：\n", summaryPath)
	printSummary(summary)
	if ask != "" {
		os.Exit(2)
	}
}

func runOnce(ctx context.Context, n int, claudePath, work string, timeout time.Duration) sample {
	s := sample{N: n, At: time.Now()}
	if a, b, c, err := loadAvg(); err == nil {
		s.Load1, s.Load5, s.Load15 = a, b, c
	} else {
		s.LoadErr = err.Error()
	}
	if du := snapshotProjects(); du.Err == "" {
		s.ProjectsBytes, s.ProjectsFiles = du.Bytes, du.Files
	} else {
		s.ProjectsErr = du.Err
	}

	t0 := time.Now()
	stats := claudeusage.ProcessStats{ExitCode: -1}
	out, _, runErr := claudeusage.Run(ctx, claudeusage.Config{
		ClaudePath: claudePath,
		WorkDir:    work,
		Timeout:    timeout,
		Env:        os.Environ(),
		OnExit: func(st claudeusage.ProcessStats) {
			stats = st
		},
	})
	s.DurationMS = time.Since(t0).Milliseconds()
	s.StdoutBytes = len(out)
	s.ExitCode = stats.ExitCode
	s.PeakRSSMB = float64(stats.PeakRSSBytes) / (1 << 20)
	if runErr != nil {
		s.Err = runErr.Error()
		return s
	}

	rep, skip, parseErr := claudeusage.ParseStream(bytes.NewReader(out))
	if parseErr != nil {
		s.Err = parseErr.Error()
		return s
	}
	res := parseResult(out)
	s.CostUSD = res.TotalCostUSD
	s.NumTurns = res.NumTurns
	s.Tokens = tokens{
		Input:         res.Usage.InputTokens,
		Output:        res.Usage.OutputTokens,
		CacheRead:     res.Usage.CacheReadInputTokens,
		CacheCreation: res.Usage.CacheCreationInputTokens,
	}
	if skip != "" {
		s.Skip = skip
		return s
	}
	s.OK = true
	s.FiveHourOK = rep.FiveHour != nil
	s.SevenDayOK = rep.SevenDay != nil
	s.RateLimitsOK = s.FiveHourOK || s.SevenDayOK
	if rep.FiveHour != nil {
		pct := rep.FiveHour.UsedPercentage
		s.FiveHourPct = &pct
	}
	if rep.SevenDay != nil {
		pct := rep.SevenDay.UsedPercentage
		s.SevenDayPct = &pct
	}
	return s
}

type summary struct {
	OutPath            string     `json:"out_path"`
	Started            time.Time  `json:"started"`
	Ended              time.Time  `json:"ended"`
	Runs               int        `json:"runs"`
	OK                 int        `json:"ok"`
	Skipped            int        `json:"skipped"`
	Errors             int        `json:"errors"`
	RateLimitsOK       int        `json:"rate_limits_ok"`
	CostTotal          float64    `json:"cost_total_usd"`
	CostMax            float64    `json:"cost_max_usd"`
	DurationMS         stat       `json:"duration_ms"`
	PeakRSSMB          float64    `json:"peak_rss_mb_max"`
	Load1Max           float64    `json:"load1_max"`
	Load1Avg           float64    `json:"load1_avg"`
	Ask                string     `json:"ask,omitempty"`
	FirstErr           string     `json:"first_err,omitempty"`
	LastAt             *time.Time `json:"last_at,omitempty"`
	ProjectsStart      diskUsage  `json:"projects_start"`
	ProjectsEnd        diskUsage  `json:"projects_end"`
	ProjectsDeltaBytes int64      `json:"projects_delta_bytes"`
}

type stat struct {
	Min int64 `json:"min"`
	Avg int64 `json:"avg"`
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	Max int64 `json:"max"`
}

func summarize(all []sample, started time.Time) summary {
	sum := summary{Started: started, Ended: time.Now(), Runs: len(all)}
	var durs []int64
	var loadSum float64
	for _, s := range all {
		if s.OK {
			sum.OK++
		}
		if s.Skip != "" {
			sum.Skipped++
		}
		if s.Err != "" {
			sum.Errors++
			if sum.FirstErr == "" {
				sum.FirstErr = s.Err
			}
		}
		if s.RateLimitsOK {
			sum.RateLimitsOK++
		}
		sum.CostTotal += s.CostUSD
		if s.CostUSD > sum.CostMax {
			sum.CostMax = s.CostUSD
		}
		durs = append(durs, s.DurationMS)
		if s.PeakRSSMB > sum.PeakRSSMB {
			sum.PeakRSSMB = s.PeakRSSMB
		}
		if s.Load1 > sum.Load1Max {
			sum.Load1Max = s.Load1
		}
		loadSum += s.Load1
		at := s.At
		sum.LastAt = &at
	}
	if len(durs) > 0 {
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		var total int64
		for _, d := range durs {
			total += d
		}
		sum.DurationMS = stat{
			Min: durs[0],
			Avg: total / int64(len(durs)),
			P50: durs[len(durs)/2],
			P95: durs[(len(durs)*95)/100],
			Max: durs[len(durs)-1],
		}
		sum.Load1Avg = loadSum / float64(len(durs))
	}
	return sum
}

func printSummary(s summary) {
	fmt.Printf("  runs=%d ok=%d skipped=%d errors=%d rate_limits_ok=%d\n",
		s.Runs, s.OK, s.Skipped, s.Errors, s.RateLimitsOK)
	fmt.Printf("  duration_ms min/avg/p50/p95/max = %d/%d/%d/%d/%d\n",
		s.DurationMS.Min, s.DurationMS.Avg, s.DurationMS.P50, s.DurationMS.P95, s.DurationMS.Max)
	fmt.Printf("  cost total/max = %.6f/%.6f；peak_rss=%.1fMB；load1 avg/max=%.2f/%.2f\n",
		s.CostTotal, s.CostMax, s.PeakRSSMB, s.Load1Avg, s.Load1Max)
	fmt.Printf("  ~/.claude/projects: %d → %d bytes (%+d)，files %d → %d\n",
		s.ProjectsStart.Bytes, s.ProjectsEnd.Bytes, s.ProjectsDeltaBytes,
		s.ProjectsStart.Files, s.ProjectsEnd.Files)
	if s.FirstErr != "" {
		fmt.Printf("  first_err=%s\n", s.FirstErr)
	}
	if s.Ask != "" {
		fmt.Printf("  ASK: %s\n", s.Ask)
	}
}

func fatalf(f string, args ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(1)
}
