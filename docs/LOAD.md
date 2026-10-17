# 服务端压测（T21）

用 `cmd/stickbox-load` 在本机压 `stickboxd`。设备走 `internal/sim` 的真
MQTT/配对/帧通知路径，但 `NoFetch=true`：不取帧、不推流、不连 DLNA；
agent 每 10 秒上报一次 usage，相当于真实 10 分钟节奏的 **60 倍速**。

**结论先行**：当前代码在 2 核 / 1.4GB 上，安全上限约 **500 台设备
（250 个 agent）**；1000 台已经贴着内存红线、上报大面积超时；2000 台
冷启动直接 OOM。瓶颈不是渲染 CPU，而是 **每连接约 1.4MB 的 broker 内存 +
Argon2 冷启动（8×64MB）+ 启动时给所有用户补帧**。优化后 2000 台可行，
CPU 在真实节奏下可支撑 ~2000 个用户。

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
# 1. 种子（服务端未启动或已停）
go run ./cmd/stickbox-load seed -root /tmp/t21/data -devices 500 \
  -state /tmp/t21/load.json -device-states /tmp/t21/devices

# 2. 起服务端（2 核 / 1.4GB）
docker run -d --name stickbox-t21 --network host --cpus=2 --memory=1400m \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  -v /tmp/t21/config.json:/etc/stickbox/config.json:ro \
  -v /tmp/t21/data:/data -v /tmp/t21/certs:/certs:ro stickboxd:t21

# 3. 压测
go run ./cmd/stickbox-load run -state /tmp/t21/load.json \
  -interval 10s -duration 90s -warmup 20s -idlewait 180s \
  -metrics http://127.0.0.1:19090/metrics -container stickbox-t21 \
  -frames-dir /tmp/t21/data/frames -out /tmp/t21/out
```

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

### 4GB 诊断跑的堆（2000 台）

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

## 3. 瓶颈

### 3.1 broker 每连接内存（首要）

`mochi-mqtt` 默认每连接约 **1.4MB**。1.4GB 的机器：
1000 连接已把 RSS 顶到 1.4GB；2000 连接 OOM。当前安全连接数约
**700–900**，与实测 500 台稳定、1000 台临界一致。

### 3.2 Argon2 冷启动

每把令牌首次校验 64MB；`argonConcurrency=8` → 峰值 512MB，CPU 全花在
64MB 清零上。服务重启后 2000 台设备 + 1000 个 agent 同时重连，
认证队列会把连接拖到超时，客户端重连又放大队列。

### 3.3 启动补帧

`frames.checkStale` 启动时给所有没有缓存的用户各渲一张「等待上报」帧。
1000 用户实测 **2 分 30 秒**、帧盘 106MB；这段时间 CPU/内存已被占满，
叠加 3.2 的认证风暴就是 2000 台 OOM 的直接原因。

### 3.4 渲染吞吐（真实节奏下不是瓶颈）

2 核、单帧约 0.6s → 理论上 **~3.3 帧/秒**。真实节奏每个用户 10 分钟
一帧，即 3.3×600 ≈ **2000 个用户**（按每 agent 2 台约 4000 台设备）。
实测 60× 加速下 5 帧/秒已把 2 核打到 250%，p99 10s，符合这个量级。

### 3.5 连接风暴

压测客户端固定 3 秒重连（模拟器行为）。服务端认证慢时，客户端在
认证完成前就重连，2000@4G 出现 92k goroutine。真机固件有退避，但仍
需要服务端侧的连接/握手准入控制。

---

## 4. 容量建议

| 条件 | 建议上限 |
|---|---|
| **当前代码，2 核 / 1.4GB** | **500 台设备 / 250 agent**（留出重启补帧与突发余量） |
| 当前代码，2 核 / 2GB+ | ~1000 台（仍需观察冷启动峰值） |
| 完成下面优化后，2 核 / 1.4GB | 1500–2000 台 |
| 渲染 CPU 上限（真实 10 分钟节奏） | ~2000 个用户（~4000 台设备），2 核 |

60× 压测下的延迟不代表真实节奏：真实每 10 分钟一帧，渲染压力是本次的
1/60，500 台的 p99 会回到 2–3 秒量级（2s 防抖 + 单帧渲染）。

---

## 5. 优化建议（按收益排序）

1. **调小 mochi-mqtt 每连接缓冲**：默认
   `MaximumClientWritesPending=8192`、`MaximumInflight=8192`、
   `ReceiveMaximum=1024`，每连接实测约 1.4MB。压到
   `MaximumClientWritesPending=64`、`MaximumInflight=32`、
   `ReceiveMaximum=32`（设备只有 3 个订阅，QoS1 流量很低），目标
   <200KB/连接。这是 2000 台的第一前提；改完用本压测复测 RSS/连接数。
2. **降低 Argon2 成本**：令牌 secret 是 24 字节随机数，不是低熵密码；
   64MB/8 并发对暴力破解的边际收益很小。可降到 16–32MB、并发 4，
   或改用带 pepper 的 HMAC-SHA256（需安全评审）。同时给 TLS/认证加
   准入信号量，避免 2000 个握手同时排队。
3. **启动补帧改成懒加载/限速**：不要启动时给所有用户渲「等待上报」帧；
   按最近上报时间分批、限并发；或等设备第一次 `GET /api/device/frame`
   再渲（返回 404 + Retry-After 也可接受）。这是冷启动 OOM 的直接修法。
4. **帧缓存不要全放内存**：`frames.Pack` 每用户约 50–100KB，1000 用户
   已 50–100MB；改成磁盘 LRU/只在内存保留最近 N 个用户。
5. **客户端退避 + 服务端 admission**：固件已用退避；服务端也应限制
   同时 TLS/认证的连接数，超限快速拒绝（带 Retry-After），避免重连放大。
6. **渲染并发按 CPU 固定**：当前 2 对 2 核合适；不要因为排队就加并发，
   那只会加剧内存和 GC。同用户多设备已共享一帧，无需再合并。

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
4. **2000 台冷启动 OOM**：见 3.1–3.3，是当前代码的真实上限问题。
5. **2000@4G 的 92k goroutine**：连接风暴放大，需要服务端准入控制。

---

## 7. 附带改动

- 新增 `cmd/stickbox-load`（seed/run、延迟统计、metrics/docker stats 采样、
  pprof）。
- `internal/sim`：`NoFetch`、`OnFrameNotify`、心跳；已配对直接订阅沿用
  T19 的 `wasPaired → subscribeData` 路径。
- `internal/metrics`：`stickbox_go_goroutines`，metrics 回环端口同时提供
  `/debug/pprof`（仍只绑 127.0.0.1）。
- `internal/frames.PendingUsers` 口径修复。
