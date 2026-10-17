# StickBox 接口文档

面向：ESP32 固件、电脑端 `stickbox-agent`、服务端运维。

StickBox 把电视变成 Claude 订阅用量看板：用户电脑上的 `stickbox-agent`
用用户自己的 Claude 登录执行 `claude -p /usage`，把结果上报给
`stickboxd`；服务端保存每个用户的最新一份，经 MQTT 推给用户绑定的屏。
设备没有手机号、没有 App：屏自己注册领密钥和配对码，agent 输码完成绑定。

- HTTP API：`https://mqtt.stickbox.ai/api`
- MQTT：`mqtts://mqtt.stickbox.ai:8883`
- 服务端：`stickboxd`（Go），内嵌 MQTT broker

> 本文只描述 T7 之后的真实接口。打印、短信、App 角色都已删除。

---

## 0. 约定

- 所有时间戳是 Unix 秒（UTC）。
- 所有 JSON 用 UTF-8，不带 BOM。
- `{dev}` 是设备 ID：**ESP32 的 STA MAC 去掉冒号的小写十六进制**，12 个字符，
  例如 `f412fa87c9e0`。全文出现的 `{dev}` 都指这个。
- 请求体、响应体都是 JSON。请求带
  `Content-Type: application/json`。
- **所有 JSON 响应都带 `Content-Length`，不用 chunked**。设备侧的
  `esp_http_client` 不解 chunked 响应体。
- 错误响应统一为：

  ```json
  {"e":"bad request","detail":"可选的人类可读说明"}
  ```

  客户端按 `e` 做分支，`detail` 只用于日志/提示，不要解析。

### 0.1 令牌

所有令牌格式相同：

```
{key_id}.{secret}
```

- `key_id`：12 个小写十六进制字符
- `secret`：32 个字符，Base64 URL 变体（`A-Z a-z 0-9 - _`，无 `=` 填充）
- 完整 45 字符，例如：

  ```
  a3f91c04bd77.kJ8xQ2mN4pL7vR1sT6wY9zA0bC3dE5fH
  ```

**当不透明字符串处理，不要解析、不要截断、不要打进日志。** 固件分配
64 字节缓冲。HTTP 请求用 `Authorization: Bearer <token>`。

### 0.2 两类主体

| 主体 | 凭证 | 能做什么 |
|---|---|---|
| 设备（ESP32） | device 令牌 | 注册/换配对码；连 MQTT 发心跳、收用量与配对通知 |
| agent（电脑） | agent 令牌 | 配对/解绑、列出名下屏、读写用量、注销 |

agent 不能连 MQTT；设备不能调 agent 的 HTTP 端点。

---

## 1. 端点总表

<!-- endpoints:start -->
| 方法 | 路径 | 身份 | 说明 |
|---|---|---|---|
| POST | `/api/device/register` | 无 | 设备注册，领 device 令牌和配对码 |
| POST | `/api/device/pair-code` | device | 配对码过期后换一个新码 |
| GET | `/api/device/frame` | device | 取当前帧包（ETag/304） |
| POST | `/api/agent/pair` | 可选 agent | 输码配对；新用户或并入已有用户 |
| POST | `/api/agent/unpair` | agent | 解绑自己名下的屏 |
| GET | `/api/agent/devices` | agent | 列出名下设备与在线状态 |
| GET | `/api/agent/usage` | agent | 读取本用户最新用量 |
| POST | `/api/usage` | agent | 上报一次用量 |
| POST | `/api/agent/logout` | agent | 注销当前 agent 令牌 |
<!-- endpoints:end -->

---

## 2. 设备

### 2.1 `POST /api/device/register`

不鉴权。设备没有密钥时（首次开机、被擦 NVS、收到解绑通知后）调用。

请求：

```json
{"dev":"f412fa87c9e0"}
```

成功 `200`：

```json
{
  "token": "a3f91c04bd77.kJ8xQ2mN4pL7vR1sT6wY9zA0bC3dE5fH",
  "code": "123456",
  "expires_at": 1762220000,
  "now": 1762219400,
  "pending": true
}
```

| 字段 | 说明 |
|---|---|
| `token` | 45 字符 device 令牌，写进 NVS `cloud/devkey`；只显示这一次 |
| `code` | 6 位数字配对码，显示在电视和 LCD 上 |
| `expires_at` | 配对码过期时间（Unix 秒），TTL 600 秒 |
| `now` | 服务端当前时间；设备用 `expires_at - now` 配合本机 uptime 算过期，不依赖设备墙钟 |
| `pending` | 恒为 `true`：这把密钥还没配对 |

行为：

- **MAC 未注册**：插入一把待配对 device 密钥，生成配对码。
- **MAC 已绑定（设备被擦 NVS 重来）**：旧的有效密钥**不吊销**；新密钥
  处于待配对。只有原主人的 agent 输码才能转正（转正时吊销旧密钥）；
  其他 agent 输码返回 `409`。这样知道 MAC 也不能把别人的屏顶掉。
- 同一台设备反复注册时，之前没配对的 pending 密钥会作废，配对码被新码替换。

错误：

| 状态 | `e` | 原因 |
|---|---|---|
| 400 | `bad dev` | `dev` 不是 12 位小写十六进制 |
| 400 | `bad request` | 请求体不是合法 JSON |
| 429 | `rate limited` | 触发限流（见第 4 节） |
| 500 | `server error` | 服务端内部错误 |

限流：每 `dev` 10 分钟 5 次；每来源 IP 10 分钟 20 次。

### 2.2 `POST /api/device/pair-code`

device 鉴权。电视/LCD 上的码过期后，用当前待配对密钥换一个新码。

请求：

```json
{}
```

成功 `200`：

```json
{"code":"654321","expires_at":1762220000,"now":1762219400}
```

- 只有 pending 密钥能换码；已经配对的密钥返回 `409 already paired`。
- 新码会替换该设备之前的码；旧码立即失效。
- 限流：每 `dev` 10 分钟 10 次。
- 错误：`400 bad dev`、`400 bad request`、`409 already paired`、
  `429 rate limited`、`500 server error`。

### 2.3 `GET /api/device/frame`

device 鉴权，且密钥必须已配对（pending 密钥返回 `403`）。

成功 `200`，`Content-Type: application/octet-stream`，body 是帧包：

```
"SBF1" | baseLen uint32 | atlasLen uint32 | metaLen uint32 | base | atlas | meta
```

三个长度都是大端。`base` / `atlas` 是 `FRAME.md` 约定的 DRI=8 JPEG 底帧与
字形图集；`meta` 是 `FRAME.md` 的 JSON 索引。

- `ETag` 是帧版本号（形如 `"7"`），同时返回 `X-Frame-Version`。
- 带 `If-None-Match` 且版本没变时返回 `304`，无 body。
- 还没出过帧时返回 `404 frame not ready`，带 `Retry-After: 30`。
- 未配对返回 `403 forbidden`。

帧版本变化时服务端往 MQTT `printer/{dev}/frame` 发 `{"ver":N}`（QoS 1，
不 retain）。设备重连订阅该主题时服务端补发当前版本。

### 2.4 MQTT 连接

```
mqtts://mqtt.stickbox.ai:8883
```

| 字段 | 值 |
|---|---|
| username | `{dev}` |
| password | device 令牌全文 |
| client_id | `{dev}` |
| keepalive | 60 秒 |
| clean_session | true |
| TLS | 系统 CA 校验，SNI = `mqtt.stickbox.ai` |

服务端在 CONNECT 时校验：

- 令牌必须是 `device` 角色；agent 令牌直接拒绝。
- 令牌里的 `dev` 必须等于 username，防止拿 A 的密钥冒充 B。

**Last Will**（设备掉线时 broker 代发，retain=1，QoS 1）：

- topic：`printer/{dev}/status`
- payload：`{"dev":"{dev}","state":"offline"}`

设备连上后订阅：

- `printer/{dev}/usage`（QoS 1）
- `printer/{dev}/pair`（QoS 1）
- `printer/{dev}/frame`（QoS 1；payload `{"ver":N}`，收到后 GET `/api/device/frame`）

并每 30 秒发布一次心跳：

- topic：`printer/{dev}/status`，QoS 1，retain=0
- payload：

  ```json
  {
    "dev": "f412fa87c9e0",
    "state": "online",
    "heap": 139000,
    "heap_min": 63548,
    "uptime": 3600
  }
  ```

  `heap` / `heap_min` / `uptime` 可选。服务端只根据「收到了合法的 status
  发布」判在线，不解析 payload 内容；连续 120 秒没有心跳即判离线。

#### 2.3.1 主题与 ACL

| topic | 方向 | retain | QoS | 说明 |
|---|---|---|---|---|
| `printer/{dev}/status` | 设备 → 服务端 | 0（LWT 为 1） | 1 | 心跳 / 掉线通知 |
| `printer/{dev}/usage` | 服务端 → 设备 | 0 | 1 | 用量推送与补发 |
| `printer/{dev}/pair` | 服务端 → 设备 | 1 | 1 | 配对 / 解绑通知 |

ACL 只有两条：

- 设备：可以 **写** 自己的 `status`；可以 **读** 自己的 `usage` 和 `pair`。
- agent：不能连 MQTT，任何 topic 都不允许。
- 不存在跨设备、通配符或 app 角色。

#### 2.3.2 usage payload

服务端推给设备的格式（`usage.DevicePayload`）：

```json
{
  "now": 1762219400,
  "ts": 1762219100,
  "five_hour": {"used_percentage": 37.5, "resets_at": 1762223000},
  "seven_day": {"used_percentage": 19.5, "resets_at": 1762395800}
}
```

| 字段 | 说明 |
|---|---|
| `now` | 服务端**现场**时间。设备没有 RTC/SNTP 时用它和本机 uptime 对表 |
| `ts` | 服务端**收到 agent 数据**的时间；显示「更新 X 分前」用它，不能用设备收到的时间，否则重连补发会把旧数据显示成刚到 |
| `five_hour` / `seven_day` | 限额窗口；没有的窗口会省略。`used_percentage` 是 0..100（服务端容忍到 1000），`resets_at` 是 Unix 秒 |

`usage` **不 retain**：payload 里的 `now` 会过期。设备订阅 `usage` 时，
服务端如果发现该设备主人已有数据，会立即现场补发一份新的（`OnSubscribed`）。
设备重连后不需要等 agent 下一次上报。

#### 2.3.3 pair payload

配对成功（retain=1）：

```json
{"paired": true, "key_id": "a3f91c04bd77"}
```

解绑（retain=1）：

```json
{"paired": false, "key_ids": ["a3f91c04bd77"]}
```

设备**只认自己当前令牌的 key_id**：

- `paired=true` 且 `key_id` 等于自己的 key_id：切到看板，清掉配对码。
- `paired=false` 且自己的 key_id 在 `key_ids` 里：清掉 NVS 里的 devkey，
  重启并重新走 `register` 出码。
- 其他通知（例如上一轮配对留下的 retain 消息）一律忽略。

固件在 token 的第一个 `.` 之前取出自己的 key_id，与通知比对。

服务端在设备**订阅** `printer/{dev}/pair` 时（`OnSubscribed`）还会按这条
连接自己的 KeyID 查库补发一条当前状态，不只依赖 retained——broker 的
retained 只在内存里，服务端一重启就没了：

- 密钥已转正：补发 `paired=true`（带自己的 `key_id`）
- 仍是 pending：不发，设备继续等 agent 输码
- 已吊销：补发 `paired=false`（`key_ids` 含自己的 `key_id`）

所以服务端重启后，已配对设备重连也能立刻拿到 `paired=true`，不会卡在
配对页。`paired=true` 可能收到两次（retained + 订阅补发），设备处理必须
幂等。

---

## 3. agent

agent 是用户电脑上的 `stickbox-agent`。所有端点都使用 agent 令牌：
`Authorization: Bearer <token>`。

### 3.1 `POST /api/agent/pair`

配对码绑定。令牌可选：

- **不带令牌**：新建一个没有手机号的用户，设备归它，返回一把新的 agent 令牌。
- **带 agent 令牌**：把新屏并入该用户（一个 agent 管多台屏），不重新签发令牌。

请求：

```json
{"code":"123456","name":"my-mac"}
```

`name` 是电脑名，可选，超过 64 字节会截断；只在新用户签发令牌时保存。

成功 `200`：

```json
{
  "dev": "f412fa87c9e0",
  "user_id": "e12f53c2bf9ef90159029db1a7811dd0",
  "token": "68ac38cb2ab9.EYnoLkaxy8FM_igVaPs3r0ZwNaIWRWjD"
}
```

`token` 只在「不带令牌、新建用户」时有值；已有 agent 配对时为空字符串。
agent 应把非空 token 存到用户配置目录（0600）。

语义：

- 配对码 6 位数字、10 分钟有效、**一次性**。服务端在开始处理时就原子地
  消费掉码，两个并发请求带同一个码只有一个能成功。
- 成功配对后服务端：
  1. 把待配对 device 密钥转正，绑定到用户；
  2. 吊销同一设备旧的有效密钥（只保留刚转正的那把）；
  3. 向 `printer/{dev}/pair` 发布 `paired=true`（retain）；
  4. 立即补发该用户最新的 `usage`（如果有）。
- **已绑定的 MAC**：其他 agent 输码返回 `409 device bound`；原主人的 agent
  输码成功并完成上面的转正/吊销。别人输错码不会把原主人的码烧掉——
  服务端会把这个仍有效的码放回去。
- 错误码：

  | 状态 | `e` | 原因 |
  |---|---|---|
  | 400 | `bad request` | JSON 非法或 `code` 不是 6 位 |
  | 401 | `unauthorized` | 带了令牌但无效/不是 agent |
  | 404 | `bad code` | 码不存在、已过期、已被用掉 |
  | 409 | `device bound` | 设备已绑定到其他用户 |
  | 409 | `bad code` | 待配对密钥已失效/已被并发转正 |
  | 429 | `rate limited` | 触发限流 |
  | 500 | `server error` | 服务端内部错误 |

限流：每来源 IP 10 分钟 10 次**失败**；全局每分钟 100 次失败。

### 3.2 `POST /api/agent/unpair`

解绑自己名下的一台屏。

请求：

```json
{"dev":"f412fa87c9e0"}
```

成功 `200`：`{"ok":1}`

- 只能解绑自己名下的设备；不是自己的返回 `403 forbidden`。
- 吊销该设备全部 device 密钥、删除配对码，并向 `printer/{dev}/pair`
  发布 `paired=false`（带被吊销的 key_ids，retain）。
- 设备收到后清 NVS 密钥、重启、重新 `register` 出码。

### 3.3 `GET /api/agent/devices`

列出本用户名下已配对的屏。

成功 `200`：

```json
{
  "devices": [
    {"dev":"f412fa87c9e0","name":"客厅","online":true,"bound":1762219400}
  ]
}
```

| 字段 | 说明 |
|---|---|
| `dev` | 设备 ID |
| `name` | 注册时留的名字，可能为空 |
| `online` | 最近 120 秒内收到过 MQTT 心跳 |
| `bound` | 当前有效 device 密钥的创建时间（Unix 秒） |

没有设备时返回 `{"devices":[]}`，不是 404。

### 3.4 `GET /api/agent/usage`

读取本用户最新一份用量。

成功 `200`：

```json
{
  "ts": 1762219400,
  "five_hour": {"used_percentage": 37.5, "resets_at": 1762223000},
  "seven_day": {"used_percentage": 19.5, "resets_at": 1762395800},
  "limits": [{"kind":"weekly_all","percent":19.5}]
}
```

- `ts` 是服务端收到 agent 数据的时间。
- `limits` 是 agent 上报的原始 limits JSON，原样返回。
- 还没有任何上报时返回 `404 not found`。

### 3.5 `POST /api/usage`

agent 上报一次用量。服务端保存为该用户的最新一份，并推给该用户名下
所有已配对的在线设备。

请求：

```json
{
  "five_hour": {"used_percentage": 37.5, "resets_at": 1762223000},
  "seven_day": {"used_percentage": 19.5, "resets_at": 1762395800},
  "limits": [{"kind":"weekly_all","percent":19.5}],
  "tz": "Asia/Bangkok"
}
```

校验：

- `five_hour` 和 `seven_day` 至少有一个。
- `used_percentage` 在 0..1000（正常是 0..100）。
- `resets_at` 必须大于 0，且不超过服务端当前时间 + 30 天。
- `limits` 原样保存，最大 8KB。
- `tz` 可选，是 agent 本机的 IANA 时区；服务端按它渲染日期/星期并判断
  跨天。没有时用 `config.json` 的 `default_tz`。
- 请求体上限 16KB。

成功 `200`：

```json
{"ok":1,"devices":1}
```

`devices` 是推送成功的台数；单台推送失败不算上报失败（数据已落库，
设备重连时会由 `usage` 订阅补发）。

错误：`400 bad request`（校验失败，`detail` 说明哪项越界）、
`401 unauthorized`、`500 server error`。

### 3.6 `POST /api/agent/logout`

注销当前 agent 令牌（只影响这一把）。

请求体可省略。成功 `200`：`{"ok":1}`。之后该令牌再请求返回 `401`。

---

## 4. 错误与限流

### 4.1 错误响应

统一格式：

```json
{"e":"unauthorized"}
```

常见 `e`：

| 状态 | `e` | 场景 |
|---|---|---|
| 400 | `bad request` | JSON 非法、字段缺失、usage 越界 |
| 400 | `bad dev` | dev 不是 12 位小写十六进制 |
| 400 | `bad code` | 配对码格式不对 |
| 401 | `unauthorized` | 无令牌、令牌无效/已吊销、角色不符 |
| 403 | `forbidden` | 操作不是自己名下的设备 |
| 404 | `not found` | 还没有用量等 |
| 404 | `bad code` | 配对码不存在、过期或已用掉 |
| 409 | `already paired` | 已配对的设备申请换配对码 |
| 409 | `device bound` | 已绑定设备被其他 agent 配对 |
| 429 | `rate limited` | 触发限流 |
| 500 | `server error` | 服务端内部错误 |

### 4.2 限流

| 动作 | 键 | 上限 |
|---|---|---|
| 设备注册 | `dev` | 10 分钟 5 次 |
| 设备注册 | 来源 IP | 10 分钟 20 次 |
| 换配对码 | `dev` | 10 分钟 10 次 |
| 配对失败 | 来源 IP | 10 分钟 10 次 |
| 配对失败 | 全局 | 1 分钟 100 次 |

来源地址归一化：

- IPv4 用完整地址。
- IPv6 按 **/64** 聚合，换一个地址绕不过每 IP 限制。
- HTTPS 走 nginx 反代时，只有 `RemoteAddr` 是回环（`127.0.0.1`/`::1`）
  才采用 `X-Forwarded-For`；直连来源自带的 `X-Forwarded-For` 一律忽略。

限流器是进程内滑动窗口，服务端重启会清零。

### 4.3 配对码生命周期

- 6 位数字，允许前导 0。
- 有效期 10 分钟。
- 一次性：成功配对即作废。
- 同一台设备同时只有一个有效码；重新出码会替换旧码。
- 过期码在查询时按「不存在」处理（`404 bad code`），并由服务端定期清理。
- 已绑定设备被其他 agent 尝试配对时返回 `409`，码会放回给原主人
  （除非设备已经换了新码）。

### 4.4 令牌缓存

服务端对令牌校验结果缓存 1 小时（argon2 只算一次）。吊销/解绑会立即
失效对应缓存；`logout` 后当前令牌马上不可用。

---

## 5. 部署拓扑

```
stickbox-agent ──HTTPS 443──▶ nginx ──TLS──▶ 127.0.0.1:9443 stickboxd
                                                       ▲
ESP32 设备 ────────────── MQTTS 8883 ───────────────────┘
```

- **HTTPS**：外网只开 443。nginx 终止 TLS，把 `/api/` 反代到本机
  `https://127.0.0.1:9443`。容器用 `network_mode: host`，
  `config.json` 的 `http_addr` 是 `127.0.0.1:9443`；9443 不对公网开放。
- **MQTT**：8883 直连，TLS 由 stickboxd 自己提供，证书与 nginx 用同一份
  Let's Encrypt 证书。
- **域名**：`mqtt.stickbox.ai`。
- 证书续期、备份、验证清单见 [`server/DEPLOY.md`](../server/DEPLOY.md)。

---

## 6. 快速联调

### 6.1 设备注册并配对

```bash
# 设备注册（真实设备由固件自己调）
curl -sS -X POST https://mqtt.stickbox.ai/api/device/register \
  -H 'Content-Type: application/json' \
  -d '{"dev":"f412fa87c9e0"}'
# → {"token":"...","code":"123456","expires_at":...,"now":...,"pending":true}

# 在电脑上输码配对（没有账号就新建一个）
curl -sS -X POST https://mqtt.stickbox.ai/api/agent/pair \
  -H 'Content-Type: application/json' \
  -d '{"code":"123456","name":"my-mac"}'
# → {"dev":"f412fa87c9e0","user_id":"...","token":"..."}
export AGENTTOK='...'
```

### 6.2 上报并读取用量

```bash
curl -sS -X POST https://mqtt.stickbox.ai/api/usage \
  -H "Authorization: Bearer $AGENTTOK" \
  -H 'Content-Type: application/json' \
  -d '{
    "five_hour":{"used_percentage":37.5,"resets_at":1762223000},
    "seven_day":{"used_percentage":19.5,"resets_at":1762395800},
    "limits":[{"kind":"weekly_all","percent":19.5}]
  }'
# → {"ok":1,"devices":1}

curl -sS https://mqtt.stickbox.ai/api/agent/usage \
  -H "Authorization: Bearer $AGENTTOK"

curl -sS https://mqtt.stickbox.ai/api/agent/devices \
  -H "Authorization: Bearer $AGENTTOK"
```

### 6.3 MQTT 心跳与订阅

```bash
# 设备心跳（username 是 dev，password 是完整 device 令牌）
mosquitto_pub -h mqtt.stickbox.ai -p 8883 --capath /etc/ssl/certs \
  -u "$DEV" -P "$DEVKEY" \
  -t "printer/$DEV/status" -q 1 \
  -m "{\"dev\":\"$DEV\",\"state\":\"online\"}"

# 订阅发给自己的用量与配对通知
mosquitto_sub -h mqtt.stickbox.ai -p 8883 --capath /etc/ssl/certs \
  -u "$DEV" -P "$DEVKEY" -q 1 \
  -t "printer/$DEV/usage" -t "printer/$DEV/pair"
```

agent 令牌不能连 MQTT；设备也不能订阅 `printer/其它dev/...`。

### 6.4 解绑

```bash
curl -sS -X POST https://mqtt.stickbox.ai/api/agent/unpair \
  -H "Authorization: Bearer $AGENTTOK" \
  -H 'Content-Type: application/json' \
  -d '{"dev":"f412fa87c9e0"}'
# → {"ok":1}
```
