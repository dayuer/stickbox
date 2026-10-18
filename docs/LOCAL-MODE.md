# 本地模式（stickbox-local）设计

> **已迁移**：本项目 2026-10-18 分拆到 dayuer/stickbox-tv，本文的后续维护在新仓库。此处仅作历史保留。

状态：**设计稿，未实施。** 任务 L0，分支 `task/local-design`。
所有三星 Room C（UA60DU7000，192.168.3.18）的参数，凡标「实测」的出自
2026-10-18 server-pull 实验（`docs/HANDOFF-server-pull.md` §1），其余一律标「待测」。

## 0. 是什么，和已有决策的关系

一个跑在用户自己电脑（Mac / Linux）上的单进程服务：

- 自己做 DLNA 控制点：SSDP 找电视 → `SetAVTransportURI` + `Play`
- 对电视输出**一条永不结束的 MPEG-TS（H.264）**：`http://<本机局域网 IP>:<port>/live.ts`
- 片源：①Claude 用量看板 ②照片 ③本地视频 ④外部直播（RTMP / HLS / RTSP）
- 一个只在局域网可达的 HTML 管理页
- **不经过 ESP32 板子**；托管模式（云端 + 板子 + MJPEG）与其协议完全不动

**与 D3 不冲突**：D3「流的起点在设备侧、不做服务端出连续视频流」的理由是
**公网**流量（`CHANNEL.md` §6：每设备每月 16–155 GB）。本地模式的出流点就在用户的
局域网里，流量不出户，那条理由不成立。D3 继续约束托管模式；本地模式由 D14/D15 约束。

## 1. 已验证的事实（决定了设计）

| 事实 | 来源 | 对设计的约束 |
|---|---|---|
| 电视从 HTTP 拉无限长 TS（`video/mpeg` + DLNA 头）能播，音视频正常 | 实测 | 输出形态就是它 |
| HLS（http/https）电视只读 m3u8 不取分片 | 实测 | **不做 HLS** |
| https 直链播不起来 | 实测 | 给电视的 URL **一律 http** |
| 同一流里字节直接拼接 → 明显暂停重载；时间戳连续的重封装 → 无缝 | 实测 | 换片必须在时间轴连续的前提下做 |
| 改 URL 再 `SetAVTransportURI` 会断一下 | 实测 | 换内容在流内完成，URL 永远不变 |
| 电视约 20 秒闪断一次流，会自己重连；用户说不用追原因 | 实测 | **重连是常态**，重连必须秒回到「现在」 |
| 4K H.264 3840×2160/30fps/20Mbps 能播 | 实测 | 4K 可选；**D13 定了先 1080p** |
| 规格：1080p30 H.264 + AAC 48k 立体声，GOP 2s，重复 SPS/PPS | 原型参数 | 默认输出规格 |
| SOAP 里只转义 `& < >`，数字实体会 402 | 实测（`memory/samsung-dlna-escaping`） | 控制点沿用 |

## 2. 进程与代码复用边界

### 2.1 形态

- **新二进制 `cmd/stickbox-local`**，Go，单文件，无 Docker、无数据库。
- 依赖一个**外部 ffmpeg**（PATH 里或配置指定）。不打包、不链接任何 GPL 编码库。
- 先做独立二进制；稳定后再考虑做成 `stickbox-agent local` 子命令（agent 已经有
  launchd / schtasks 安装代码和 Claude 用量采集，合进去能少装一个东西）。先独立的理由：
  agent 是常驻、极轻、给托管模式用的；本地模式要常驻一个编码进程，故障面不同，别连坐。

### 2.2 复用什么、抽什么

| 现有包 | 怎么用 | 说明 |
|---|---|---|
| `internal/render` + `internal/theme` | **原样复用** | `render.Quota{R}.FullImage(snap)` 出 1920×1080 整帧。不需要 SBF1 帧包/字形图集——本地模式由编码器出流，不在设备上拼带 |
| `internal/source` | **原样复用** `Claude` 换算与 `Snapshot` | 时区用 `NewFixedTZ` 或本机时区 |
| `internal/photo` 的 `process.go` / `exif.go` | **原样复用** `Process()`：转正、缩到 1080p、剥 EXIF | `manifest.go` / `service.go` 依赖 `store`，**不复用** |
| `internal/tvcheck` 的 `Discover()` + `internal/sim` 的 SOAP | **复用**为控制点 | 已在三星上跑通 SSDP / SetAVTransportURI / GetTransportInfo |
| `cmd/stickbox-agent` 的 `/usage` 采集 | **抽出**成 `internal/claudeusage` | 现在是 `package main` 私有函数（`parseUsageStream` 等），agent 与 local 共用 |
| `internal/frames`、`store`、`broker`、`httpapi` | **不复用** | 那是多租户云端的缓存/MQTT/鉴权，本地单用户用不上 |

新增 `internal/local/...`（第 3–6 节）。**抽包是唯一会改到现有代码的地方**，
要单独提交、单独审，且只移动不改行为。

### 2.3 用量数据怎么来（本地模式不经服务器）

托管模式：agent 跑 `claude -p /usage` → `POST /api/usage` → 云端渲染 → 板子取帧。
**本地模式直接在进程内取，不上报任何服务器**：

```
internal/claudeusage（从 agent 抽出，agent 与 local 共用同一份）
  └─ 白名单环境 + 专用空工作目录里执行 claude -p /usage --setting-sources project
     --output-format stream-json --verbose
  └─ parseUsageStream → usage.Report（five_hour / seven_day，0..100 + resets_at）
        │
        ▼
source.Claude{Report, ReceivedAt}.Snapshot(now)   ← 与云端同一个换算
        │
        ▼
render.Quota{R}.FullImage(snap)  → 混合器的「看板」片源
```

- **沿用 agent 已踩过的坑**（agent 的 `filterEnv`、`memory/t2-agent-local-usage-testing`）：
  必须白名单环境 + 专用空目录，否则 `rate_limits` 为 null。这些逻辑随包一起搬，不重写
- **频率**：默认 5 分钟一次，与 agent 一致。`/usage` 是本地斜杠命令；按 T2 的结论不调用模型，
  但本地模式要长期常驻跑，用 L-E0 复核一次（第 9 节），确认不产生费用
- **没在用 Claude 时**：看板停在上一次的数据，「更新 X 分前」如实显示（用户已确认这是对的）
- **可选增强（不在首版）**：`docs/experiments/claude-usage/` 已验证 statusline stdin 带
  `rate_limits`，交互会话期间可以更及时地刷新；只在同一台机器上、只读本地数据
- **不用**：claude.ai 网页 Cookie、`api.anthropic.com/api/oauth/usage`（理由见
  `docs/experiments/claude-usage/README.md`）
- 本地模式**不需要配对、不需要服务端账号**。托管模式的 agent 照旧上报，两者互不影响；
  同一台机器同时跑两者时各自采集（`/usage` 很轻，重复一次可接受，L-E0 一并确认）

## 3. 视频管线

### 3.1 核心决定：在编码前切换，只有一个编码器

```
片源解码器（每个片源一个，按需起停）        混合器（Go，主时钟）        编码器（一个，常驻）
  看板：Go 渲染整帧（每分钟一次）  ─┐
  照片：Go 解码 JPEG              ─┼─▶ 每 1/30s 取「当前片源」最新一帧 ─▶ ffmpeg ─▶ TS 扇出 ─▶ 电视
  本地视频：ffmpeg → rawvideo     ─┤    每 1/48000s 取对应音频样本        rawvideo+PCM in   (/live.ts)
  直播：ffmpeg → rawvideo + PCM   ─┘    片源欠帧就重复上一帧 / 补静音      H.264+AAC TS out
```

为什么不沿用原型的「预切 2 秒段 + concat 流拷贝」：

| | concat 流拷贝（原型） | 编码前切换（本设计） |
|---|---|---|
| 素材规格 | **必须统一**，否则 concat 崩 | 任意：解码后统一缩放/重采样 |
| 直播源 | 还是得先实时转码成统一规格的段 | 天然支持 |
| 时间戳连续 | 靠 ffmpeg 重封装 | **天然连续**：编码器只看到一条从不中断的输入 |
| 电视重连接回「现在」 | 原型没做到（从素材第 0 段重来） | 天然：编码器一直在跑，新连接从最近的 IDR 接入 |
| 切换延迟 | 1–2 段（2–4 秒） | 下一帧（约 33ms），见 3.4 |
| CPU | 低（只封装） | 一路 1080p30 实时编码（硬编很低，软编约一核，**待测**） |

本地模式在用户自己的电脑上、局域网里，**一路常驻编码换掉了规格统一、重连续播、
直播转码三件麻烦事**，这笔账划算。concat 方案作为「电脑太弱」时的退路写进第 8 节。

### 3.2 混合器（`internal/local/mix`）

- 自己是主时钟：严格按 30fps 产出视频帧、按 48kHz 产出音频样本，**不跟随任何片源的时钟**
- 每个片源实现「给我当前帧」：看板和照片永远有帧（静止图）；视频和直播从环形缓冲取，
  **欠帧就重复上一帧、欠音频就补静音**，绝不让编码器输入断流
- 输出：视频 `yuv420p 1920×1080` 写编码器 stdin；音频 `s16le 48k 立体声` 写第二个管道（fd 3）
- 1080p yuv420p 一帧约 3.1MB，30fps 约 93MB/s，本机管道没问题

### 3.3 编码器（`internal/local/enc`）

外部 ffmpeg。分辨率/帧率/GOP/音频取原型实测规格；**码率、编码后端、profile 待测**（L-E3、L-E6）：

```
ffmpeg -f rawvideo -pix_fmt yuv420p -s 1920x1080 -r 30 -i pipe:0 \
       -f s16le -ar 48000 -ac 2 -i pipe:3 \
       -c:v <后端> -g 60 -keyint_min 60 -sc_threshold 0 -bf 0 \
       -profile:v high -b:v 6M -maxrate 8M -bufsize 8M \
       -c:a aac -b:a 128k -ar 48000 -ac 2 \
       -f mpegts -mpegts_flags resend_headers pipe:1
```

| 后端 | 用法 | 备注 |
|---|---|---|
| macOS `h264_videotoolbox` | **默认** | 硬编，CPU 低；SPS/PPS 重复、profile/level 是否被三星接受**待测** |
| Linux `h264_vaapi` / `h264_v4l2m2m` | 有就用 | 待测 |
| `libx264 -preset veryfast -tune zerolatency -x264-params repeat-headers=1` | 兜底 | 外部进程调用，不涉及我们打包 GPL |

码率（**全部待测**，L-E6）：静止看板/照片用不满码率，6M 是给视频和直播的建议上限，**局域网带宽不是约束**，
真正要测的是电视解码器吃不吃得下（待测，3–8 Mbps 区间各测一档）。

编码器挂了 → 混合器丢帧不阻塞，2 秒内重启编码器；已连接的电视会断，靠它自己重连接回。

### 3.4 TS 扇出与重连（`internal/local/tsfan`）

- 编码器 stdout 是一条 TS。扇出层按 188 字节切包，缓存**最近一个 GOP**
  （从最后一个带 `random_access_indicator` 的视频包开始），以及最新 PAT/PMT
- 新连接（电视重连也算）：先发 PAT/PMT，再从缓存的 IDR 开始，然后跟实时
- 结果：电视每 20 秒闪断一次时，重连立刻拿到「现在」的画面，而且是从关键帧起
- **待测**：重连从「最近 IDR」而不是「新的 IDR」开始时，电视是否接受时间戳（缓存的
  GOP 最多晚 2 秒）。不行就改为等下一个 IDR 再放流（最多 2 秒黑）

切换延迟：换片发生在混合器里，编码器看到的只是「下一帧的输入变了」，照常编成 P 帧，
所以画面在**下一帧**就换、无黑屏、无需等 IDR。GOP（2 秒）只决定**重连接入点**的间隔。

## 4. 片源抽象（`internal/local/src`）

```go
type Source interface {
    ID() string
    Kind() Kind                 // Board / Photo / Video / Live
    // Prepare 在轮到它之前提前调用（预热解码器、预读首帧）。幂等。
    Prepare(ctx context.Context) error
    // Frame/Audio 被混合器每帧调用，不得阻塞；没有新帧就返回上一帧。
    Frame() *Frame
    Audio(dst []int16) int      // 返回填了多少样本，不足由混合器补静音
    State() State
    Stop()
}
```

状态机：

```
Idle ──Prepare──▶ Starting ──首帧到达──▶ Ready ──解码器退出/超时──▶ Failed ──退避到点──▶ Starting
                     │ 超时（看板/照片不会超时）                         │
                     └────────────────────────────────▶ Failed ──────┘
```

- **混合器只切到 Ready 的片源**。播放列表提前 `Prepare` 下一项（直播提前 5 秒，视频 1 秒），
  到点没 Ready 就跳过或延长当前项——这是「不黑屏」的第二道保证
- **直播断了**：`Ready → Failed`，混合器当帧切回**兜底片源**（默认：用量看板），叠一行
  「直播中断，重连中」（看板模板加一个状态位，不另画画面）；退避 2/5/15/60 秒重连，
  恢复后如果仍在它的时段就切回
- 本地视频播完：按播放列表规则下一项或循环
- 片源永远不能让混合器卡住：解码器的读写都在自己的 goroutine，混合器只读环形缓冲

播放列表：

```json
{
  "items": [
    {"source": "board",  "secs": 60},
    {"source": "photos", "secs": 8, "each": true},
    {"source": "live:cctv13", "until": "21:00"}
  ],
  "fallback": "board",
  "schedule": [{"days": "mon-fri", "from": "09:00", "to": "18:00", "items": ["board"]}]
}
```

- `each`：照片目录里每张轮一次 8 秒
- `schedule`：按时段换整份列表；没有匹配时段用默认 `items`
- 管理页「立即播放」= 临时覆盖，直到用户取消或该项结束

## 5. HTML 管理页

### 5.1 功能

- 电视：发现列表、选定/锁定一台、当前传输状态、「重推」按钮
- 片源：照片（上传、删除、看缩略图）、本地视频（选目录/文件，显示规格与时长）、
  直播（添加 URL、测试连通性——只在本机拉 5 秒解码，不推电视）、看板（预览当前帧）
- 播放列表：增删排序、时长、时段规则、「立即播放」
- 状态：编码器后端、当前码率/帧率、已连接客户端、最近 20 条事件
- 预览：管理页里一个 `<img>` 每 2 秒刷新混合器当前帧（JPEG），**不在浏览器里拉 TS**

### 5.2 技术选型

- `go:embed` 的静态 HTML + 原生 JS + 一个 CSS 文件，**无前端构建**、无 npm
- JSON API：`/api/tv`、`/api/sources`、`/api/playlist`、`/api/status`、`/api/preview.jpg`
- 照片上传复用 `photo.Process()`

### 5.3 访问控制

| 口 | 默认绑定 | 鉴权 | 原因 |
|---|---|---|---|
| 流 `/live.ts` | 局域网 IP | **无**（电视不能鉴权）但路径带随机 token：`/s/<32 位随机>/live.ts` | 电视只认 URL；随机路径防局域网里随手扫 |
| 管理页 + API | **`127.0.0.1`** | 本机访问不要口令 | 默认只能本机打开 |
| 管理页走局域网（可选） | 用户显式开启 `admin_lan: true` | 必须设口令；会话 cookie `HttpOnly; SameSite=Strict`；所有写接口要 `X-CSRF-Token` | 手机上管理的需求 |

- **任何情况下不绑 `0.0.0.0` 的公网口，不做 UPnP 端口映射**
- 启动日志打印实际监听的地址，管理页显示「流地址可被局域网访问」

## 6. 电视管理

- 发现：`tvcheck.Discover()`（SSDP M-SEARCH `MediaRenderer`），结果按 UDN 去重
- 锁定：配置里记 UDN + 最后一次的 IP；IP 变了按 UDN 重新找
- 推送：`SetAVTransportURI(流地址, DIDL)` + `Play`，DIDL 只转义 `& < >`
- 看门狗：每 5 秒 `GetTransportInfo`；`STOPPED` / `NO_MEDIA_PRESENT` 连续 15 秒才重推，
  指数退避。`PLAYING ↔ TRANSITIONING` 不算失败（实测会闪断）
- 电视关机：发现不到就停止重推、降低探测频率；编码器空闲（无客户端）**10 分钟后停**省 CPU，
  电视回来再起（起播会慢几秒，待测）
- **先只支持一台**。多台：同一条流推给多台在技术上可以（扇出天然支持多连接），
  但同步偏差、各自看门狗都没测过，另立任务

## 7. 存储、配置、日志、自启

| 系统 | 数据目录 |
|---|---|
| macOS | `~/Library/Application Support/StickBox Local/` |
| Linux | `$XDG_DATA_HOME/stickbox-local/`（默认 `~/.local/share/stickbox-local/`） |

```
config.json      监听地址、电视 UDN、编码后端、ffmpeg 路径、admin 口令哈希
playlist.json    第 4 节格式
photos/          已处理的 1080p JPEG（内容寻址，文件名 = sha256）
videos.json      本地视频的引用（只存路径，不复制文件）
live.json        直播源 URL（可能含凭据：文件权限 0600）
logs/            按天滚动，保留 7 天
```

- 配置变更原子写（临时文件 + rename）
- 日志不记直播 URL 里的凭据、不记管理口令
- 自启：macOS 用户级 launchd plist；Linux `systemd --user` 单元。**复用 agent 已有的
  launchd 安装代码**（`cmd/stickbox-agent/main.go` 的 bootstrap/bootout 路径）

## 8. 风险与未知

1. **单编码器方案没在电视上验证过**。原型验的是 concat 流拷贝；「Go 混合器 → 一个 ffmpeg」
   的输出在三星上是否同样无缝，是第一个要测的
2. **VideoToolbox 输出的兼容性**：profile/level、SPS/PPS 重复方式与 x264 不同
3. **电视对「无音频轨」的态度**：静止片源要不要强制带静音 AAC。设计上默认一直带
4. **重连接入点**：从缓存 IDR 接入时间戳回退约 2 秒，电视接不接受
5. **CPU**：一路 1080p30 常驻编码在目标机器上的占用；电脑睡眠时流断，用户要知道
6. **直播源的多样性**：RTMP/HLS/RTSP 的分辨率、帧率、音频格式五花八门，解码端统一缩放/
   重采样能不能稳；有些源有 DRM 或地域限制，那是做不了的
7. **电视空闲/待机行为**：电视待机后的唤醒、输入源被用户切走时的传输状态
8. **退路**：如果单编码器在弱机器上扛不住，退回原型的「预转码统一段 + concat 流拷贝」，
   只支持 ①②③，④直播需要单独实时转码一路

## 9. 实验清单（按优先级；全部由 USB-TV 调度）

规则：每个实验产物**先在本机验证**再上电视——`ffprobe -show_frames` 确认无 DTS 回退、
GOP=60、每个 IDR 前有 SPS/PPS；`ffmpeg -v warning -i out.ts -f null -` 无告警。
未经本机验证的文件不推电视（D13 教训）。

| 优先级 | # | 实验 | 通过标准 | 不过的后果 |
|---|---|---|---|---|
| **P0** | L-E1 | 单编码器 + 静止图（看板截图）推 30 分钟 | 持续出画，无卡顿；记录 CPU | 第 3 节整体方案回退到 concat 流拷贝 |
| **P0** | L-E2 | 同一流内 看板 → 照片 → 本地视频 → 看板 切换 | 目视无黑屏、无暂停重载 | 同上 |
| **P0** | L-E0 | 本机（不上电视）24 小时每 5 分钟跑一次 `claude -p /usage` | 账号用量/费用无增长；每次都拿到 `rate_limits` | 降低频率或改走 statusline |
| P1 | L-E3 | VideoToolbox vs libx264 各推 10 分钟 | 都能播；不能播的记 profile/level | 默认后端改 libx264 |
| P1 | L-E4 | 无音频轨 vs 静音 AAC 轨 | 确认是否必须带音频 | 决定混合器是否常带静音轨（默认带） |
| P1 | L-E5 | 等电视 20 秒闪断 / 人为断开，看重连 | 重连后是「现在」的画面；记录黑屏时长 | 接入点改为等下一个 IDR（最多 2 秒黑） |
| P2 | L-E7 | 接一个 RTMP/HLS 直播，播放中断网 | 秒切兜底片源，恢复后自动切回 | 调片源状态机超时 |
| P2 | L-E6 | 码率 3 / 6 / 8 Mbps 各一档播本地视频 | 电视不掉帧的上限 | 调默认码率 |
| P3 | L-E8 | 电视待机 / 切走输入源再回来 | 看门狗重推回来的用时 | 调看门狗阈值 |
| P3 | L-E9 | 编码器空闲停后电视回来的起播时间 | 记录秒数 | 调空闲停时长或不停 |

**P0 三条任何一条不过，先停下来改设计，不进入实现。**

## 10. 不做

- 不做 HLS、不给电视 https 地址、不改 URL 换内容
- 不碰 `rom/`，不改托管模式任何协议
- 不打包 GPL 编码库；ffmpeg 由用户自备
- 不做公网访问、不做端口映射
- 多电视、4K（D13）另立任务
