# claude `/usage` 24h 采样（L-E0 / issue #6）

本目录是本机实验工具，不参与服务端构建。目的：验证每 5 分钟一次
`claude -p /usage` 的代价与稳定性，为托管模式的用量采集定预算。

## 做什么

- 串行执行（上一次退出后才开始下一次），默认间隔 5 分钟、单次超时 90 秒。
- 子进程单独成组；超时/取消时整组 `SIGKILL`，正常退出后也清理同组残留，
  不留孤儿。stdout/stderr 接临时文件，避免孙进程攥着管道把 `Wait` 拖死。
- 峰值内存取内核记账的 `rusage.ru_maxrss`，不是 `ps` 采样。
- 环境变量走 `internal/claudeusage` 的白名单，工作目录是
  `~/Library/Application Support/stickbox/work` 这个专用空目录，
  并带 `--no-session-persistence`。
- 每轮记录：耗时、退出码、`rate_limits` 是否非空、5h/7d 百分比、
  `total_cost_usd`、`num_turns`、tokens、峰值 RSS、系统 1/5/15 负载、
  `~/.claude/projects` 的体积与文件数。
- 日志只写 `/tmp/claudeusage-<时间>.jsonl` 和同名 `.summary.json`，
  不入库、不上报。

## 安全阈值

- 任一轮 `total_cost_usd > 0` → 打印 `ASK` 并停止。
- 连续 3 轮 `rate_limits` 为空 → 打印 `ASK` 并停止。

## 跑法

```sh
# 短验证（3 轮，15 秒间隔）
go run . --runs 3 --interval 15s

# 24h 长跑：必须用 caffeinate 包住，否则拒绝启动
caffeinate -dimsu go run . --duration 24h
```

编译后跑更稳：

```sh
go build -o /tmp/claudeusage-harness .
caffeinate -dimsu /tmp/claudeusage-harness --duration 24h
```

长跑期间本机每 10 分钟还有 stickbox-agent 的用量采集在并行跑（用户要求恢复
监测），所以负载会叠加；报告里要注明。

## 结构

- `main.go` / `main_test.go`：实验 harness。
- `go.mod`：独立小模块，通过 `replace` 指向 `../../../server/go`，
  复用 `internal/claudeusage`，不复制实现。
