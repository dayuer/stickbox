# 交接单：服务端出流，板子只做 DLNA 控制点

状态：**原型已全部在电视上验证通过（2026-10-18）**，本文是把它落进固件和服务端的任务书。
原型脚本在 `probe/dlna-tv/tools/`，实测结论见第 1 节。

## 1. 已验证的事实（三星 UA60DU7000，用户目视确认）

| 项 | 结论 |
|---|---|
| 电视从服务端 HTTP 拉 MP4 | 能播（H.264 1080p60） |
| HLS（http 和 https） | **不能用**：电视只读 m3u8，一个分片都不取 |
| 无限长 MPEG-TS（`video/mpeg` + DLNA 头） | 能播，音视频正常 |
| 同一条流里换素材 | 字节直接拼接 → 明显暂停重载；ffmpeg concat 流拷贝重封装 → **无缝** |
| 运行中随时换片 | `live_switch.py` 验证：2 秒分段 + concat 符号链接清单，全程单 GET，延迟约 2 秒 |
| 4K H.264（3840×2160/30fps/20Mbps） | 能播，画面确为 4K |
| 电视约 20 秒闪断一次流 | 用户明确说**不用管** |

**所以电视不吃 HLS，也不能把 URL 换了再 `SetAVTransportURI`（会断一下）**；
服务端必须对电视始终输出同一条 `/live.ts`，换内容在流内完成。

## 2. 分工与边界

- **esp**：固件（`rom/`）。**esp1**：服务端（`server/`）。各自在 `task/*` 分支，不碰对方目录。
- 两边约定的接口只有第 3 节那一条 MQTT 消息，先照着写，有不清楚的找 USB-TV 会话定。
- **板子和电视是共享资源，开发会话不要烧录、不要推电视**；上板验收由 USB-TV 会话排期做。
  需要上板时在交付说明里写明「请上板验证」及要看的现象。
- 合并和 push main 只有 USB-TV 会话做。

## 3. 接口：MQTT `printer/{dev}/play`

QoS 1，不 retain。服务端发，设备收（和 `printer/{dev}/frame` 同一套订阅/鉴权流程，
配对成功后订阅，见 `rom/main/cloud_client.c` 的 `subscribe_paired_topics()`）。

```json
{"url": "http://192.168.3.237:8770/live.ts", "mime": "video/mpeg", "title": "StickBox"}
```

- `url` 必须是 **http**（电视不吃 https），指向服务端的一条永不结束的 TS 流。
- `url` 为空串 = 停止，设备回到本机 MJPEG（`/live.avi`）兜底。
- 设备处理完往 `printer/{dev}/ack` 回一条（沿用现有 ack 格式，`kind` 用 `play`），
  成功/失败都要回；失败带原因（没找到电视 / 推送被拒）。

## 4. 任务 A（esp，固件）

现状：`rom/main/main.c` 的 `supervisor_task` 找电视后推**本机** `http://<板子>:STREAM_PORT/live.avi`（`video/avi`）。
`rom/main/dlna.c` 已有 `dlna_find_tv / dlna_push / dlna_stop`，**SOAP 流程不用重写**。

要做的：

1. `cloud_client.c`：订阅 `printer/{dev}/play`，解析后通知 supervisor（队列或事件组，别在 MQTT 任务里做 DLNA ——
   参见 `a7bed38`：解绑/失效重启要移出 MQTT 任务，同类原因）。
2. `main.c` supervisor：有 play URL 时用它 `dlna_push(&tv, url, mime, title)`，没有或被清空时回到本机 `live.avi`。
   本机 MJPEG 服务保留，作离线兜底。
3. **重推判据要改**：现在用本机 `stream_clients()` 判断电视是否还在拉流，对外部 URL 不成立。
   加一个 `dlna_transport_state()`（`GetTransportInfo` → `CurrentTransportState`），
   STOPPED / NO_MEDIA_PRESENT 超过 N 秒就重推（沿用现有指数退避）。电视会闪断，PLAYING→TRANSITIONING→PLAYING 不算失败。
4. URL 持久化到 NVS（重启后继续推），解绑/重新注册时清掉。
5. 堆：这条路径不经板子数据通路，堆压力应该比 MJPEG 路径小；`memdiag` 日志保留，交付时附上对账数字。

SOAP 转义铁律（`memory/samsung-dlna-escaping`）：DIDL 里只转义 `& < >`，**不要用数字实体**（电视回 402）。

验收：

- 主机侧：`idf.py build` 通过；不引入新的静态内存增长超过 2KB。
- 上板（由 USB-TV 做）：发一条 play 消息，电视在 ≤5 秒内出画；拔掉服务端后板子不崩、回到兜底；
  清空 URL 后回到本机 MJPEG；重启后恢复。

## 5. 任务 B（esp1，服务端）

把 `probe/dlna-tv/tools/live_switch.py` 的做法产品化，放进 `server/`：

1. **频道流端点**：`GET /channel/{id}/live.ts`，对电视输出永不结束的 TS。
   做法照 `live_switch.py`：ffmpeg `-re -f concat` 读一份很长的清单，每项是符号链接，读到第 k 段才决定它指向谁；
   `-c copy` 流拷贝，**不重新编码**。头部必须带 `Content-Type: video/mpeg`、
   `transferMode.dlna.org: Streaming`、`contentFeatures.dlna.org: DLNA.ORG_OP=00;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000`。
2. **素材入库统一规格**：1080p30（或 4K30）H.264 high + AAC 48kHz 立体声，GOP 2 秒（`-g 60 -sc_threshold 0 -bf 0`），
   `repeat-headers=1`，按关键帧切 2 秒段（`-f segment -segment_time 2 -reset_timestamps 1`）。规格不一致的先转码。
3. **切换接口**：设置某个频道当前内容（先做内部函数 + 一个调试用 HTTP），延迟目标 ≤ 2 个分段。
4. **电视重连续播**：电视会断开重连。重连后应接在「频道时间线上现在的位置」，
   不是从素材第 0 段重来（原型没做这一条，`live_switch.py` 里重连从当前素材第 0 段开始）。
5. **下发**：设备绑定到频道后，往 `printer/{dev}/play` 发第 3 节那条消息；`url` 用设备所在局域网能访问到的地址
   （自建 = 局域网地址；租用 = 要先想清楚电视到公网的路径，**先别做，文档里写明未决**）。
6. 参考 `docs/CHANNEL.md`（`task/T31-channel-decision` 分支）的频道模型，和那份文档保持一致；有冲突先找 USB-TV。

验收：

- 单元/集成测试：切换内容时，同一个连接里输出的 TS 无时间戳回退（`ffmpeg -v warning -i out.ts -f null -` 无 DTS 乱序/不连续警告）。
- 离线冒烟：`curl` 拉 30 秒落盘，中途切两次，总时长 ≈ 30 秒。
- 上电视（由 USB-TV 做）：全程单 GET，换片无缝，延迟约 2 秒。

## 6. 不做

- 不做 HLS。不做 https 流地址。不改电视的 URL 来换内容。
- 不追电视 20 秒闪断的原因。
- 双电视同步、板子管多台，另立任务。
