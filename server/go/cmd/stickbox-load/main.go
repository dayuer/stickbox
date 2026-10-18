// stickbox-load 是本机压测工具：N 台模拟设备 + M 个 agent。
//
//	stickbox-load seed -root /tmp/t21/data -devices 2000 -state /tmp/t21/load.json
//	stickbox-load run  -state /tmp/t21/load.json -duration 120s -out /tmp/t21/out
//
// 设备复用 internal/sim 的真 MQTT/配对/帧通知路径，但 NoFetch=true：
// 不取帧、不推流、不连 DLNA，只测服务端的渲染与通知。agent 按
// -interval（默认 10s，相当于真实 10 分钟的 60 倍速）上报 usage。
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/sim"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

type agentState struct {
	UserID  string   `json:"user_id"`
	Token   string   `json:"token"`
	Devices []string `json:"devices"`
}

type loadState struct {
	Server          string       `json:"server"`
	MQTTAddr        string       `json:"mqtt_addr"`
	DeviceStatesDir string       `json:"device_states_dir"`
	Agents          []agentState `json:"agents"`
}

type deviceFile struct {
	Server   string `json:"server"`
	MQTTAddr string `json:"mqtt_addr"`
	Dev      string `json:"dev"`
	Token    string `json:"token"`
	Paired   bool   `json:"paired"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "seed":
		seed(os.Args[2:])
	case "run":
		run(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法:
  stickbox-load seed -root <data> -devices N [-agents M] [-state file] [-device-states dir] [-server URL] [-mqtt host:port]
  stickbox-load run  -state file [-interval 10s] [-duration 120s] [-warmup 20s] [-metrics URL] [-container name] [-frames-dir dir] [-out dir]`)
	os.Exit(2)
}

func seed(args []string) {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	root := fs.String("root", "/tmp/t21/data", "服务端 data 目录（jobs.db 所在）")
	devices := fs.Int("devices", 100, "设备数 N")
	agents := fs.Int("agents", 0, "agent 数 M；0=约每 agent 2 台")
	statePath := fs.String("state", "/tmp/t21/load.json", "输出 load 状态（含令牌，0600）")
	deviceDir := fs.String("device-states", "/tmp/t21/devices", "每台设备的 sim 状态目录")
	server := fs.String("server", "https://127.0.0.1:19443", "服务端 URL")
	mqttAddr := fs.String("mqtt", "127.0.0.1:18883", "MQTT host:port")
	pepper := fs.String("pepper", "", "必须与服务端 config.json 的 token_pepper 相同")
	fs.Parse(args)

	if *devices <= 0 {
		fatal("devices 必须 > 0")
	}
	m := *agents
	if m <= 0 {
		m = (*devices + 1) / 2
	}
	if m > *devices {
		m = *devices
	}
	if err := os.MkdirAll(*root, 0o755); err != nil {
		fatal(err.Error())
	}
	st, err := store.Open(filepath.Join(*root, "jobs.db"))
	if err != nil {
		fatal("打开数据库: " + err.Error())
	}
	defer st.Close()
	if *pepper == "" {
		fatal("必须给 -pepper（与服务端 config.json 的 token_pepper 一致）")
	}
	v := auth.NewVerifier(st, *pepper)

	out := loadState{
		Server: *server, MQTTAddr: *mqttAddr, DeviceStatesDir: *deviceDir,
		Agents: make([]agentState, m),
	}
	for i := range out.Agents {
		u, err := st.CreateUser()
		if err != nil {
			fatal("建用户: " + err.Error())
		}
		tok, err := auth.IssueAgentKey(v, st, u.ID, fmt.Sprintf("load-agent-%d", i))
		if err != nil {
			fatal("签发 agent 令牌: " + err.Error())
		}
		out.Agents[i] = agentState{UserID: u.ID, Token: tok}
	}

	for d := 0; d < *devices; d++ {
		ai := d % m
		dev := fmt.Sprintf("%012x", d+1)
		tok, _, err := auth.IssueDeviceKey(v, st, out.Agents[ai].UserID, dev, "", false)
		if err != nil {
			fatal("签发设备密钥: " + err.Error())
		}
		dir := filepath.Join(*deviceDir, dev)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fatal(err.Error())
		}
		raw, _ := json.Marshal(deviceFile{
			Server: *server, MQTTAddr: *mqttAddr, Dev: dev, Token: tok, Paired: true,
		})
		if err := os.WriteFile(filepath.Join(dir, "device.json"), raw, 0o600); err != nil {
			fatal(err.Error())
		}
		out.Agents[ai].Devices = append(out.Agents[ai].Devices, dev)
	}

	raw, _ := json.MarshalIndent(out, "", "  ")
	if err := os.MkdirAll(filepath.Dir(*statePath), 0o700); err != nil {
		fatal(err.Error())
	}
	if err := os.WriteFile(*statePath, raw, 0o600); err != nil {
		fatal(err.Error())
	}
	fmt.Printf("seeded %d devices / %d agents -> %s\n", *devices, m, *statePath)
}

type sample struct {
	At         time.Time `json:"at"`
	Pending    float64   `json:"frame_pending_users"`
	MQTT       float64   `json:"mqtt_connections"`
	Online     float64   `json:"online_devices"`
	Goroutines float64   `json:"go_goroutines"`
	CPUPercent float64   `json:"cpu_percent,omitempty"`
	RSSMB      float64   `json:"rss_mb,omitempty"`
}

type results struct {
	Devices    int                `json:"devices"`
	Agents     int                `json:"agents"`
	IntervalS  float64            `json:"interval_s"`
	DurationS  float64            `json:"duration_s"`
	ReportsOK  int64              `json:"reports_ok"`
	ReportsErr int64              `json:"reports_err"`
	Timeouts   int64              `json:"frame_notify_timeouts"`
	LatencyMS  map[string]float64 `json:"frame_notify_latency_ms"`
	ConnectMS  map[string]float64 `json:"mqtt_connect_latency_ms"`
	Connects   int64              `json:"mqtt_connects"`
	CPUTimeS   float64            `json:"server_cpu_time_s,omitempty"`
	CPUPercent float64            `json:"server_cpu_percent,omitempty"`
	Samples    []sample           `json:"samples"`
	FramesKB   int64              `json:"frames_disk_kb"`
	Final      string             `json:"final_metrics"`
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	statePath := fs.String("state", "/tmp/t21/load.json", "seed 输出的状态文件")
	interval := fs.Duration("interval", 10*time.Second, "agent 上报间隔（真实 10 分钟的压缩）")
	duration := fs.Duration("duration", 120*time.Second, "稳态时长")
	warmup := fs.Duration("warmup", 20*time.Second, "等 MQTT 连接/订阅的预热")
	idleWait := fs.Duration("idlewait", 240*time.Second, "等启动补帧跑完（frame_pending_users=0）")
	metricsURL := fs.String("metrics", "http://127.0.0.1:19090/metrics", "stickboxd metrics URL")
	container := fs.String("container", "", "可选：docker 容器名，用于采样 CPU/RSS")
	framesDir := fs.String("frames-dir", "/tmp/t21/data/frames", "帧缓存目录（统计磁盘占用）")
	outDir := fs.String("out", "/tmp/t21/out", "结果输出目录")
	waitTimeout := fs.Duration("timeout", 30*time.Second, "等 frame 通知的上限")
	serverPID := fs.Int("server-pid", 0, "服务端进程 PID；给了就统计这段时间的 CPU")
	fs.Parse(args)

	raw, err := os.ReadFile(*statePath)
	if err != nil {
		fatal(err.Error())
	}
	var st loadState
	if err := json.Unmarshal(raw, &st); err != nil {
		fatal("解析 state: " + err.Error())
	}
	if len(st.Agents) == 0 {
		fatal("state 里没有 agent")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal(err.Error())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wallStart := time.Now()
	cpuStart, haveCPU := processCPUTime(*serverPID)

	devAgent := map[string]int{}
	agentCh := make([]chan time.Time, len(st.Agents))
	for i := range agentCh {
		agentCh[i] = make(chan time.Time, 1)
	}
	for ai, ag := range st.Agents {
		for _, dev := range ag.Devices {
			devAgent[dev] = ai
		}
	}

	var connectMu sync.Mutex
	var connectLat []time.Duration
	devices := 0
	for _, ag := range st.Agents {
		for _, dev := range ag.Devices {
			devices++
			d, err := sim.New(sim.Config{
				Server: st.Server, MQTTAddr: st.MQTTAddr,
				StateDir: filepath.Join(st.DeviceStatesDir, dev),
				Insecure: true, NoFetch: true,
				Logf: func(string, ...any) {},
				OnConnected: func(dev string, d time.Duration) {
					connectMu.Lock()
					connectLat = append(connectLat, d)
					connectMu.Unlock()
				},
				OnFrameNotify: func(dev string, ver uint64) {
					if idx, ok := devAgent[dev]; ok {
						select {
						case agentCh[idx] <- time.Now():
						default:
						}
					}
				},
			})
			if err != nil {
				fatal("创建 sim " + dev + ": " + err.Error())
			}
			go d.Run(ctx)
		}
	}
	fmt.Printf("devices=%d agents=%d interval=%s duration=%s\n", devices, len(st.Agents), *interval, *duration)

	// 预热：等 MQTT 连接数接近设备数（或超时）。
	waitMQTT(ctx, *metricsURL, devices, *warmup)
	// 服务端启动时会给没有帧缓存的用户补一张「等待上报」帧；等它跑完，
	// 否则测到的是启动补帧 + 上报渲染的混合负载。
	if d := waitIdle(ctx, *metricsURL, *idleWait); d > 0 {
		fmt.Printf("启动补帧在 %s 内完成\n", d.Round(time.Second))
	}

	var (
		mu       sync.Mutex
		samples  []sample
		finalTxt string
	)
	stopSampler := make(chan struct{})
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		var lastCPU sample
		_ = lastCPU
		for {
			select {
			case <-stopSampler:
				return
			case <-tick.C:
			}
			s := fetchSample(*metricsURL)
			if *container != "" {
				cpu, rss := dockerStats(*container)
				s.CPUPercent, s.RSSMB = cpu, rss
			}
			mu.Lock()
			samples = append(samples, s)
			mu.Unlock()
		}
	}()

	// pprof：稳态末尾抓 goroutine 和 5 秒 CPU profile。
	pprofDone := make(chan struct{})
	go func() {
		defer close(pprofDone)
		select {
		case <-time.After(*duration):
		case <-ctx.Done():
			return
		}
		grabPprof(*metricsURL, *outDir)
	}()

	type agentResult struct {
		lat  []time.Duration
		ok   int64
		err  int64
		late int64
	}
	resultsByAgent := make([]agentResult, len(st.Agents))
	var wg sync.WaitGroup
	reportCtx, reportCancel := context.WithTimeout(ctx, *duration)
	defer reportCancel()

	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			MaxIdleConns:        len(st.Agents) + 10,
			MaxIdleConnsPerHost: len(st.Agents) + 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	for ai, ag := range st.Agents {
		wg.Add(1)
		go func(ai int, ag agentState) {
			defer wg.Done()
			tick := time.NewTicker(*interval)
			defer tick.Stop()
			rep := 0
			for {
				select {
				case <-reportCtx.Done():
					return
				case <-tick.C:
				}
				drain(agentCh[ai])
				start := time.Now()
				rep++
				code, err := postUsage(client, st.Server, ag.Token, ai, rep)
				if err != nil || code != 200 {
					resultsByAgent[ai].err++
					continue
				}
				select {
				case nt := <-agentCh[ai]:
					resultsByAgent[ai].lat = append(resultsByAgent[ai].lat, nt.Sub(start))
					resultsByAgent[ai].ok++
				case <-time.After(*waitTimeout):
					resultsByAgent[ai].late++
				}
			}
		}(ai, ag)
	}

	<-reportCtx.Done()
	wg.Wait()
	close(stopSampler)
	samplerWG.Wait()
	<-pprofDone

	var all []time.Duration
	res := results{
		Devices: devices, Agents: len(st.Agents),
		IntervalS: interval.Seconds(), DurationS: duration.Seconds(),
		LatencyMS: map[string]float64{},
	}
	for _, r := range resultsByAgent {
		res.ReportsOK += r.ok
		res.ReportsErr += r.err
		res.Timeouts += r.late
		all = append(all, r.lat...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	res.LatencyMS["count"] = float64(len(all))
	if len(all) > 0 {
		res.LatencyMS["p50"] = ms(percentile(all, .50))
		res.LatencyMS["p99"] = ms(percentile(all, .99))
		res.LatencyMS["max"] = ms(all[len(all)-1])
		res.LatencyMS["avg"] = ms(avg(all))
	}
	mu.Lock()
	res.Samples = samples
	mu.Unlock()
	connectMu.Lock()
	conn := append([]time.Duration(nil), connectLat...)
	connectMu.Unlock()
	sort.Slice(conn, func(i, j int) bool { return conn[i] < conn[j] })
	res.Connects = int64(len(conn))
	res.ConnectMS = map[string]float64{}
	if len(conn) > 0 {
		res.ConnectMS["p50"] = ms(percentile(conn, .50))
		res.ConnectMS["p95"] = ms(percentile(conn, .95))
		res.ConnectMS["p99"] = ms(percentile(conn, .99))
		res.ConnectMS["max"] = ms(conn[len(conn)-1])
		res.ConnectMS["avg"] = ms(avg(conn))
	}
	if haveCPU {
		if cpuEnd, ok := processCPUTime(*serverPID); ok {
			wall := time.Since(wallStart)
			res.CPUTimeS = cpuEnd.Seconds() - cpuStart.Seconds()
			if wall > 0 {
				res.CPUPercent = res.CPUTimeS / wall.Seconds() * 100
			}
		}
	}
	res.FramesKB = dirSizeKB(*framesDir)
	res.Final = fetchText(*metricsURL)
	_ = finalTxt

	out, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "results.json"), out, 0o644); err != nil {
		fatal(err.Error())
	}
	if res.Final != "" {
		_ = os.WriteFile(filepath.Join(*outDir, "metrics-final.txt"), []byte(res.Final), 0o644)
	}
	printSummary(res)
}

// waitIdle 等启动补帧真正停下来。
//
// 懒加载下，渲染要等设备连上并订阅后才开始；只看「某一刻 pending==0」
// 会在订阅风暴之前就误判。先给 15s 观察窗，等看到忙碌，再要求连续 6s
// 空闲才返回。
func waitIdle(ctx context.Context, metricsURL string, max time.Duration) time.Duration {
	start := time.Now()
	seenBusy := false
	var idleSince time.Time
	for time.Since(start) < max {
		select {
		case <-ctx.Done():
			return time.Since(start)
		case <-time.After(2 * time.Second):
		}
		if fetchSample(metricsURL).Pending > 0 {
			seenBusy = true
			idleSince = time.Time{}
			continue
		}
		if !seenBusy && time.Since(start) < 15*time.Second {
			continue
		}
		if idleSince.IsZero() {
			idleSince = time.Now()
			continue
		}
		if time.Since(idleSince) >= 6*time.Second {
			return time.Since(start)
		}
	}
	return time.Since(start)
}

func waitMQTT(ctx context.Context, metricsURL string, want int, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		s := fetchSample(metricsURL)
		if int(s.MQTT) >= int(float64(want)*0.9) {
			fmt.Printf("MQTT 已连接 %d/%d\n", int(s.MQTT), want)
			return
		}
	}
	fmt.Println("预热超时，继续（连接数可能未满）")
}

// processCPUTime 读进程累计 CPU 时间（user+sys）。用 ps -o time=，
// macOS/Linux 都支持；解析 [[dd-]hh:]mm:ss[.ss]。
func processCPUTime(pid int) (time.Duration, bool) {
	if pid <= 0 {
		return 0, false
	}
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, false
	}
	var days int
	if i := strings.IndexByte(s, '-'); i >= 0 {
		days, _ = strconv.Atoi(s[:i])
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var h, m int
	var sec float64
	if len(parts) == 3 {
		h, _ = strconv.Atoi(parts[0])
		m, _ = strconv.Atoi(parts[1])
		sec, _ = strconv.ParseFloat(parts[2], 64)
	} else {
		m, _ = strconv.Atoi(parts[0])
		sec, _ = strconv.ParseFloat(parts[1], 64)
	}
	return time.Duration(days)*24*time.Hour +
		time.Duration(h)*time.Hour +
		time.Duration(m)*time.Minute +
		time.Duration(sec*float64(time.Second)), true
}

func fetchSample(metricsURL string) sample {
	txt := fetchText(metricsURL)
	s := sample{At: time.Now()}
	for _, line := range strings.Split(txt, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "stickbox_frame_pending_users":
			s.Pending = v
		case "stickbox_mqtt_connections":
			s.MQTT = v
		case "stickbox_online_devices":
			s.Online = v
		case "stickbox_go_goroutines":
			s.Goroutines = v
		}
	}
	return s
}

func fetchText(u string) string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func postUsage(c *http.Client, server, token string, ai, rep int) (int, error) {
	now := time.Now().Unix()
	body := fmt.Sprintf(
		`{"five_hour":{"used_percentage":%.1f,"resets_at":%d},`+
			`"seven_day":{"used_percentage":%.1f,"resets_at":%d}}`,
		float64(10+(ai+rep)%80), now+4*3600,
		float64(15+(ai*3+rep)%75), now+6*86400)
	req, err := http.NewRequest("POST", strings.TrimRight(server, "/")+"/api/usage",
		strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

func drain(ch chan time.Time) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func dockerStats(container string) (cpu, rssMB float64) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream",
		"--format", "{{.CPUPerc}}\t{{.MemUsage}}", container).Output()
	if err != nil {
		return 0, 0
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(parts) == 2 {
		cpu, _ = strconv.ParseFloat(strings.TrimSuffix(parts[0], "%"), 64)
		rssMB = parseMemMB(parts[1])
	}
	return
}

func parseMemMB(s string) float64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	num := f[0]
	mult := 1.0
	switch {
	case strings.HasSuffix(num, "GiB"):
		mult, num = 1024, strings.TrimSuffix(num, "GiB")
	case strings.HasSuffix(num, "MiB"):
		mult, num = 1, strings.TrimSuffix(num, "MiB")
	case strings.HasSuffix(num, "KiB"):
		mult, num = 1.0/1024, strings.TrimSuffix(num, "KiB")
	}
	v, _ := strconv.ParseFloat(num, 64)
	return v * mult
}

func grabPprof(metricsURL, outDir string) {
	u, err := url.Parse(metricsURL)
	if err != nil {
		return
	}
	base := u.Scheme + "://" + u.Host
	client := &http.Client{Timeout: 30 * time.Second}
	if resp, err := client.Get(base + "/debug/pprof/goroutine?debug=1"); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		_ = os.WriteFile(filepath.Join(outDir, "goroutine.txt"), b, 0o644)
	}
	if resp, err := client.Get(base + "/debug/pprof/heap"); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		_ = os.WriteFile(filepath.Join(outDir, "heap.pprof"), b, 0o644)
	}
	if resp, err := client.Get(base + "/debug/pprof/profile?seconds=5"); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		_ = os.WriteFile(filepath.Join(outDir, "cpu.pprof"), b, 0o644)
	}
}

func dirSizeKB(root string) int64 {
	var total int64
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total / 1024
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func avg(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	var sum time.Duration
	for _, v := range d {
		sum += v
	}
	return sum / time.Duration(len(d))
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func printSummary(r results) {
	var maxPending, maxMQTT, maxGor, maxCPU, maxRSS float64
	for _, s := range r.Samples {
		if s.Pending > maxPending {
			maxPending = s.Pending
		}
		if s.MQTT > maxMQTT {
			maxMQTT = s.MQTT
		}
		if s.Goroutines > maxGor {
			maxGor = s.Goroutines
		}
		if s.CPUPercent > maxCPU {
			maxCPU = s.CPUPercent
		}
		if s.RSSMB > maxRSS {
			maxRSS = s.RSSMB
		}
	}
	fmt.Printf("reports ok=%d err=%d notify-timeout=%d\n", r.ReportsOK, r.ReportsErr, r.Timeouts)
	fmt.Printf("frame notify latency ms: p50=%.0f p99=%.0f max=%.0f avg=%.0f (n=%.0f)\n",
		r.LatencyMS["p50"], r.LatencyMS["p99"], r.LatencyMS["max"],
		r.LatencyMS["avg"], r.LatencyMS["count"])
	fmt.Printf("peak: mqtt=%.0f pending=%.0f goroutines=%.0f cpu=%.0f%% rss=%.0fMB frames=%.1fMB\n",
		maxMQTT, maxPending, maxGor, maxCPU, maxRSS, float64(r.FramesKB)/1024)
	if r.Connects > 0 {
		fmt.Printf("mqtt connect latency ms: p50=%.1f p95=%.1f p99=%.1f max=%.1f avg=%.1f (n=%d)\n",
			r.ConnectMS["p50"], r.ConnectMS["p95"], r.ConnectMS["p99"],
			r.ConnectMS["max"], r.ConnectMS["avg"], r.Connects)
	}
	if r.CPUPercent > 0 {
		fmt.Printf("server cpu: %.1f%% (%.1fs cpu over the run)\n", r.CPUPercent, r.CPUTimeS)
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "stickbox-load:", msg)
	os.Exit(1)
}
