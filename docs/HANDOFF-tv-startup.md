# T17：电视首次出画延迟

状态：自动实验完成第一轮；实际出画时刻待用户配合；HTTP/AVI 剩余变体待电视空闲。
板子全程停在下载模式，未改 `rom/`。

## 1. 现象与目标

用户实测从点开到画面出现约 15 秒。目标：**出画 ≤3 秒，之后一直播、不出现 STOPPED/断开/重连**。

## 2. 环境与方法

- Mac（192.168.3.237）起 HTTP 服务，DLNA `SetAVTransportURI` + `Play` 推 `video/avi`。
- 电视：三星 UA60DU7000（Room C，192.168.3.18）。
- 测试帧：T13a 的 1920×1080 baseline JPEG，181KB
  （`esp-airprint-T13a/docs/design/preview/01-超速.spliced.jpg`）。
- 工具：`docs/experiments/tvstart/exp2.py`（自包含，含 `live_avi.py`、`dlna_push.py`）。
  它记录 HEAD/GET、GET 请求头、发帧进度、每 0.25s 的 `GetTransportInfo`。
- 每次先 `Stop`，等 2 秒再推；每次换端口，避免 TIME_WAIT。
- 16:44 板子被意外复位过一次，之前的数据作废；下表全部是 16:45 之后、板子停在
  `esptool --after no_reset` 时的干净数据。

```bash
python3 -I docs/experiments/tvstart/exp2.py --tv 192.168.3.18 \
  --burst 8 --fps 2 --duration 20 --frame <frame.jpg>
```

## 3. 实验矩阵（第一轮）

### 3.1 开机突发帧数（2fps，181KB）

| burst | GET | PLAYING | 流断开/STOPPED |
|---|---|---|---|
| 0 | 2.38s | 5.78s | 无（70s 基线） |
| 2 | 2.44s | 5.05s | 无 |
| 4 | 2.23s | 3.83s | 无 |
| 8 | 1.54s | 2.08s | 无 |
| 8（复测） | 1.68s | 2.50s | 无 |
| 16 | 0.21s | 0.97s | 无 |

- `GET` 是电视发起拉流的时刻；`Play` 返回后电视固定等 1.5–2.4 秒才 GET。
- burst 是 GET 之后**不等间隔连发**的帧数，之后回到 2fps。
- burst 8 首突发约 1.45MB；burst 16 约 2.9MB。
- **PLAYING 随 burst 单调下降，8 帧已进 2–2.5 秒，16 帧进 1 秒。**

### 3.2 帧大小（burst 8，2fps）

| 帧 | PLAYING |
|---|---|
| 82KB（q25） | 3.62s |
| 181KB（q82） | 2.08–2.50s |
| 520KB（q100） | 2.21s |

没有明显的字节依赖，差异在电视自身的缓冲/状态抖动范围内（±1s）。
小帧反而慢，说明电视不是按字节数决定起播。

### 3.3 帧率（burst 8，181KB）

| fps | PLAYING |
|---|---|
| 1 | 2.55s |
| 2 | 2.08–2.50s |
| 5 | 2.81s |
| 10 | 3.08s |

高帧率略微更慢（8 帧覆盖的视频时长更短）。2fps 合适。

### 3.4 AVI 头字段（burst 8，2fps，181KB）

| 变体 | 改动 | PLAYING |
|---|---|---|
| base | 帧数/movi = BIG | 2.08–2.50s |
| totalframes0 | `dwTotalFrames=0` | 2.58s |
| totalframes100k | `dwTotalFrames=100000` | 2.08s |
| maxbps0 | `dwMaxBytesPerSec=0` | 2.21s |
| maxbpsbig | `dwMaxBytesPerSec=BIG` | 2.26s |

没有实质差异。AVI 头保持现状即可。

### 3.5 真实数据长跑（USB-TV，burst 8 / 2fps）

17:00:06 开始推送，17:00:07 GET，17:00:08 PLAYING（约 2 秒）。
之后连续约 **6 分钟**只有这一次 GET：没有 STOPPED、没有 BrokenPipe、
没有重连；每分钟换帧（服务端用真实数据重渲染 + 拼时钟）都没断流。
帧 180–188KB，参数与本页推荐一致。这基本排除了「18 秒必断」是固定周期行为。

### 3.6 电视的请求头

HEAD：

```
Host, getcontentFeatures.dlna.org: 1, getCaptionInfo.sec: 1
```

GET：

```
Host, getcontentFeatures.dlna.org: 1, transferMode.dlna.org: Streaming,
getMediaInfo.sec: 1, Connection: close
```

没有 `Range` / `TimeSeekRange`，也没有要求 `Content-Length`。

## 4. 关于「18 秒 STOPPED / 断开」

干净环境下**没有复现**：

- 70 秒 burst 0 基线：一直 PLAYING，无 BrokenPipe。
- 6 分钟真实数据长跑（burst 8）：一次 GET，无 STOPPED/BrokenPipe/重连。
- 20 秒短测：STOPPED 出现在约 22 秒，是**我们自己的流结束**（duration=20s）导致的，
  不是电视主动断。
- 之前 7.2s BrokenPipe / 17.9s PLAYING / 18s STOPPED 的观测，时间上与 16:44 板子
  复位撞车；板子 app 可能同时向电视推流，电视同一时间只接一路，状态被搅乱。
  该组数据已作废。

仍需验证的假设（等电视空闲）：

1. 电视缓冲填满后主动断开、播完缓冲再停（需要 >2 分钟长跑 + 状态记录）。
2. `Content-Length` / HTTP/1.1 / DLNA OP/FLAGS 触发不同缓冲策略。
3. 电视在「缓冲 → 播放」阶段切换时短暂 STOPPED，但画面不断。

如果长跑再现，记录：相对开始时间、服务器是否 `BrokenPipe`、`GetTransportInfo`
序列、电视是否自动重连 GET。

## 5. 推荐推流参数

| 项 | 推荐 |
|---|---|
| 开机突发 | **burst 8**（约 2s 出 PLAYING）；要 ≤1s 用 16 |
| 帧率 | 2 fps |
| AVI 头 | 保持现状（BIG 帧数/movi，`dwMaxBytesPerSec=maxframe*fps`） |
| HTTP | `HTTP/1.0 200`、无 `Content-Length`、`Connection: close` |
| DLNA | `transferMode.dlna.org: Streaming`、`OP=00`、`FLAGS=017000…` |
| Content-Type | `video/avi` |

首突发只在每次电视 GET 时发生；之后按 2fps 常速，不额外占带宽。

## 6. 固件 `stream.c` 建议（只写建议，未改 `rom/`）

在 `client_task` 的发送循环里加一个开机突发，其余不动：

```c
#define STARTUP_BURST 8

/* ... 发完一帧、n++ 之后 ... */
if (n > STARTUP_BURST) {
    int64_t due = t0 + (int64_t)(n - STARTUP_BURST) * 1000000 / s_fps;
    int64_t wait = due - esp_timer_get_time();
    if (wait > 0) vTaskDelay(pdMS_TO_TICKS(wait / 1000));
}
```

- 前 8 帧连续发同一张 flash 帧（约 1.45MB），之后 2fps。
- 不需要新缓冲；`send_all` 直接读 mmap，`s_busy` 保护条带双缓冲。
- 建议在日志里记录「突发 8 帧耗时」和「GET → 首帧」，上板后核对 Mac 的 2s 是否成立。
- AVI 头、HTTP 头、DLNA 头都不要改。
- 电视重连时 `server_task` 会 accept 新连接并新起 `client_task`，现有结构已支持；
  每次新连接都会重新突发 8 帧，这正是需要的。

## 7. 待办

1. **实际出画时刻**：PLAYING 不等于画面出来。需要用户看着电视，在出画瞬间示意；
   计划用带秒数的帧/明显颜色切换做一次 60 秒推送，由 USB-TV 协调。
2. HTTP/AVI 剩余变体（Content-Length、HTTP/1.1、OP/FLAGS、transferMode、
   suggested buffer、length0、riff0、bisizejpg）——等 USB-TV 长跑结束。
3. 10 分钟以上长跑，确认无 STOPPED/断开。
4. 上板验证固件突发：先 `esptool --after watchdog_reset` 恢复板子，再测。

## 8. 板子与电视状态

- 板子停在下载模式：`esptool.py -p /dev/cu.usbmodem1301 --after no_reset chip_id`
  （**不带 `--after` 默认 `hard_reset`，会把 app 放起来**）。
  恢复：`esptool.py -p /dev/cu.usbmodem1301 --after watchdog_reset chip_id`。
- 电视已 `Stop`，当前 `STOPPED`；T17 实验期间由 USB-TV 接管做真实数据长跑。
