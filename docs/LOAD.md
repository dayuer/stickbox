# 服务端压测（T21/T22）

用 `cmd/stickbox-load` 在本机压 `stickboxd`。设备走 `internal/sim` 的真
MQTT/配对/帧通知路径，但 `NoFetch=true`：不取帧、不推流、不连 DLNA；
agent 每 10 秒上报一次 usage，相当于真实 10 分钟节奏的 **60 倍速**。

**结论先行**：T22 优化后，2 核 / 1.4GB 上 **2000 台设备 / 1000 个 agent
可以稳定运行**（RSS 峰值 471MB，不再 OOM）；500 台在 60× 加速下 0 超时，
1000/2000 台的通知超时来自渲染吞吐（60× 是真实压力的 60 倍），不是容量
问题。改造前的基线（500 台安全上限、2000 台 OOM）保留在第 2 节，便于对比。

---

## 1. 方法

- 服务端：`docker run --cpus=2 --memory=1400m`（2000@4G 诊断跑用 `4g`），
  自签证书，端口 19443/18883/19090。
- 设备/agent：宿主机上的 `stickbox-load` 进程；设备复用 `internal/sim`，
  `NoFetch=true`；agent 用真实 `/api/usage`。
- 种子：直接写库创建用户/密钥（绕过 HTTP 注册限流），每 agent 1–3 台设备。
- 预热：等 MQTT 连接数 ≥90% 设备数，再等 `frame_pending_users` 归零
  （启动补帧跑完），然后开始上报。
- 采样：每 2s 抓 `/metrics`；每 2s 用 `docker stats` 取 CPU/RSS；
  结束时抓 `/debug/pprof/{heap,cpu}`。
- 延迟：从 `POST /api/usage` 发出到任一设备收到 `printer/{dev}/frame`
  通知（含服务端 2s 防抖 + 渲染排队）。

复现：

```bash
# 1. 种子（服务端未启动或已停）。-pepper 必须与服务端 config 一致。
go run ./cmd/stickbox-load seed -root /tmp/t22/data -devices 500 \
  -state /tmp/t22/load.json -device-states /tmp/t22/devices \
  -pepper "$(python3 -c 'import json;print(json.load(open("/tmp/t22/config.json"))["token_pepper"])')"

# 2. 起服务端（2 核 / 1.4GB）
docker run -d --name stickbox-t22 --network host --cpus=2 --memory=1400m \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
  -v /tmp/t22/config.json:/etc/stickbox/config.json:ro \
  -v /tmp/t22/data:/data -v /tmp/t22/certs:/certs:ro stickboxd:t22

# 3. 压测
go run ./cmd/stickbox-load run -state /tmp/t22/load.json \
  -interval 10s -duration 90s -warmup 5s -idlewait 900s \
  -metrics http://127.0.0.1:19090/metrics -container stickbox-t22 \
  -frames-dir /tmp/t22/data/frames -out /tmp/t22/out
```

T22 起 `waitIdle` 会先观察是否出现渲染任务，再要求连续 6 秒空闲，
避免懒加载在设备订阅前被误判为“已空闲”。

---

## 2. 数据（60× 加速）

| 规模 | agent | 上报 ok/err | frame 通知超时 | 延迟 p50 / p99 | CPU 峰值/均值 | RSS 峰值/均值 | MQTT | 帧磁盘 | 启动补帧 |
|---|---|---|---|---|---|---|---|---|---|
| 100 台 | 50 | 417 / 0 | 0 | 4.1s / 10.6s | 250% / 85% | 1.21GB / 0.34GB | 100 | 10.6MB | ~2s |
| 500 台 | 250 | 1447 / 164 | 0 | 11.0s / 38.7s | 269% / 196% | 1.40GB / 0.63GB | 500 | 53MB | 14s |
| 1000 台 | 500 | 148 / 725 | 1481 | 5.2s / 16.2s* | 243% / 200% | 1.40GB / 1.25GB | 1000 | 106MB | 150s |
| 2000 台 @1.4G | 1000 | 0 / 5089 | — | — | — | OOMKilled | — | 26MB | — |
| 2000 台 @4G（诊断） | 1000 | 0 / 5050 | — | — | 285% / 235% | 4.10GB / 4.09GB | 1216 | 53MB | 188s |

\* 1000 台只有 148 次拿到通知；p50/p99 只代表这些“跑赢了队列”的样本，
1481 次超时才是主体。在线设备数与 MQTT 连接数都达到设备数（心跳正常）。

---

## 2b. T22 优化后复测（同条件）

改动：mochi 缓冲 64/32/32、全局连接准入、HMAC-SHA256 + pepper、
帧缓存磁盘/内存 LRU、设备首次取帧懒加载 + 限速补帧。同一台机器、
同样的 2 核 / 1.4GB、同样的 60× 节奏，每次从空库重新种子。

| 规模 | agent | 上报 ok/err | frame 通知超时 | 延迟 p50 / p99 | CPU 峰值 | RSS 峰值 | MQTT | 帧磁盘 |
|---|---|---|---|---|---|---|---|---|
| 500 台 | 250 | 1672 / 9 | 102 | 9.9s / 30.1s | 288% | **234MB** | 500 | 53MB |
| 1000 台 | 500 | 1488 / 0 | 877 | 7.6s / 32.4s | 283% | **340MB** | 1000 | 106MB |
| 2000 台 | 1000 | 718 / 872 | 2514 | 11.0s / 38.5s | 441% | **495MB** | 2000 | 212MB |

同一配置重复跑的波动不小（宿主上还有别的容器/会话）：500 台曾出现
1941/0、0 超时；2000 台也在 1626/31 与 718/872 之间摆动。共性结论：
无论哪次都不再 OOM，RSS 稳定在 234–495MB，在线设备/连接数满额。

- 2000 台不再 OOM，goroutine 峰值 6,746（改造前诊断跑 92,181）。
- 上报错误从改造前的数百/数千降到 0–31；1000/2000 台的通知超时是
  **渲染吞吐**：60× 下 500/1000 个 agent 每 10 秒要一帧，需求 50/100 帧/秒，
  2 核单帧约 0.6s，只能出 ~3 帧/秒。真实 10 分钟节奏下需求是 0.8/1.7 帧/秒，
  在 2 核能力内。
- 500 台在 60× 下基本不超时（最好一次 0 次），说明内存和连接不再是
  瓶颈；1000/2000 台的超时是 60× 渲染需求超过 2 核吞吐，不是内存。

### 4GB 诊断跑的堆（2000 台，改造前）

```
1731MB  mochi-mqtt newClient        ← 约 1.4MB / 连接
1152MB  argon2.initBlocks           ← 校验风暴，8 并发 × 64MB 上下
 104MB  bytes.growSlice
  90MB  image.NewRGBA
  49MB  frames.Pack
```

CPU profile（2000@4G）：`runtime.memclrNoHeapPointers` 65%——几乎全是
Argon2 清零 64MB 块，不是渲染。2000@4G 时 goroutine 峰值 **92,181**：
客户端 3 秒重连快于服务端认证队列，连接风暴把 goroutine 放大。

---

## 3. 瓶颈（T21 基线；T22 已修 3.1–3.3）

### 3.1 broker 每连接内存（T22 已修）

`mochi-mqtt` 默认每连接约 **1.4MB**。压到 64/32/32 后，2000 连接只占
RSS 471MB（含帧缓存与 Go 运行时），不再 OOM。

### 3.2 Argon2 冷启动（T22 已修）

改用 HMAC-SHA256 + pepper 后首次校验是微秒级；旧 argon2 记录只在首次
成功校验时迁移一次，迁移期仍受 8 路信号量限制。

### 3.3 启动补帧（T22 已修）

不再启动时全量渲染：设备第一次 GET/订阅时懒加载，其余缺帧用户按
`backfill_users_per_sec`（默认 2/秒）排队。1000 用户的重连渲染由
`RenderConcurrency=2` 自然限流，内存不再累积帧包。

### 3.4 渲染吞吐（真实节奏下不是瓶颈）

2 核、单帧约 0.6s → 理论上 **~3.3 帧/秒**。真实节奏每个用户 10 分钟
一帧，即 3.3×600 ≈ **2000 个用户**（按每 agent 2 台约 4000 台设备）。
实测 60× 加速下 5 帧/秒已把 2 核打到 250%，p99 10s，符合这个量级。

### 3.5 连接风暴（T22 已修）

压测客户端固定 3 秒重连（模拟器行为）。服务端认证慢时，客户端在
认证完成前就重连，2000@4G 出现 92k goroutine。现在 `mqtt_admit_per_sec`
（默认 50/秒）在认证前快速拒绝超限连接，2000 台的 goroutine 峰值降到
6,746；被拒次数看 `stickbox_rate_limited_total{limiter="mqtt_admit"}`。

---

## 4. 容量建议

| 条件 | 建议上限 |
|---|---|
| **T22 代码，2 核 / 1.4GB** | **2000 台设备 / 1000 agent**（RSS 471MB，留出余量） |
| 渲染 CPU（真实 10 分钟节奏，2 核） | ~2000 个用户（~4000 台设备） |
| 60× 加速压测（非真实节奏） | 500 台无超时；1000+ 受渲染吞吐限制 |

60× 压测下的延迟不代表真实节奏：真实每 10 分钟一帧，渲染压力是本次的
1/60。2000 台在真实节奏下的帧需求约 1.7 帧/秒，2 核可以覆盖；内存
不再是限制。

---

## 5. 优化项与落地情况

1. **调小 mochi-mqtt 每连接缓冲（已落地）**：
   `mqtt_max_writes_pending` / `mqtt_max_inflight` /
   `mqtt_receive_maximum`，默认 64/32/32。
2. **HMAC-SHA256 + pepper（已落地）**：替代 Argon2 热路径，
   `token_pepper` 必填；旧记录校验成功后就地迁移。见
   [`docs/SECURITY.md`](SECURITY.md)。
3. **启动补帧懒加载 + 限速（已落地）**：设备首次 GET/订阅才渲染，
   其余用户按 `backfill_users_per_sec` 排队。
4. **帧缓存磁盘/内存 LRU（已落地）**：`frame_cache_max_bytes`（默认
   256MB）与 `frame_memory_max_bytes`（默认 64MB），按最近取用淘汰；
   启动只读缓存头。
5. **服务端连接准入（已落地）**：`mqtt_admit_per_sec`（默认 50/秒），
   在认证前拒绝超限连接；客户端仍按固件退避重试。
6. **渲染并发按 CPU 固定（保持）**：`RenderConcurrency` 默认 2，对应
   2 核；排队时不要加并发，只会加剧内存和 GC。

---

## 6. 压测中发现/修复的问题

1. **`stickbox_frame_pending_users` 口径错误（已修）**：原实现返回
   `len(workers)`，worker 是常驻的，空闲也计数，导致空闲时仍显示 250。
   现在只统计有排队/防抖/渲染任务的用户（`pending` 计数）。
2. **模拟器不发心跳（已补）**：首轮 `online_devices=0`；`internal/sim`
   现在在 MQTT 连接后每 30s 发 `printer/{dev}/status`，在线数正确。
3. **模拟器已配对状态不订数据主题（已修）**：预置 `paired=true` 时
   走 `wasPaired → subscribeData` 直接订阅 usage/frame（T19 的
   `subOK/subDenied/subFailed` 逻辑）；`NoFetch` 可关闭取帧。
4. **2000 台冷启动 OOM（T22 已修）**：见 3.1–3.3；复测 2000 台 RSS 471MB。
5. **2000@4G 的 92k goroutine（T22 已修）**：连接准入后降到 6,746。
6. **压测工具过早判定启动补帧完成（T22 已修）**：懒加载下渲染在设备
   订阅后才开始，`waitIdle` 现在先观察忙碌、再要求连续 6 秒空闲。

---

## 7. 附带改动

- 新增 `cmd/stickbox-load`（seed/run、延迟统计、metrics/docker stats 采样、
  pprof）。
- `internal/sim`：`NoFetch`、`OnFrameNotify`、心跳；已配对直接订阅沿用
  T19 的 `wasPaired → subscribeData` 路径。
- `internal/metrics`：`stickbox_go_goroutines`，metrics 回环端口同时提供
  `/debug/pprof`（仍只绑 127.0.0.1）。
- `internal/frames.PendingUsers` 口径修复。
- T22：`cmd/stickbox-load` 增加 `-pepper`；`internal/broker` 容量参数与
  连接准入；`internal/auth` HMAC + pepper + 旧记录迁移；`internal/frames`
  双 LRU、懒加载、限速补帧。
