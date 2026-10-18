# 电视推流实验脚本

两条研究线，八个脚本。每个脚本的用法和结论写在它自己的 docstring 里，
这里只列「哪个脚本回答哪个问题」，免得下次要翻八个文件才找得到入口。

全部脚本都用 `python3 -I` 跑（隔离模式，不从脚本目录和当前目录加载第三方包）。

## 公共模块

| 文件 | 作用 |
|---|---|
| `dlna_push.py` | DLNA 控制点：SSDP 发现、`SetAVTransportURI` / `Play` / `Stop`、`GetTransportInfo` |
| `live_avi.py` | 永不结束的 MJPEG-AVI 流（这是**固件今天在用**的那种封装） |

`live_ts.py` 和 `push_url.py` 会 `import dlna_push`，所以这些脚本必须放在同一个目录里。

## 线一：首次出画延迟（T17）

结论见 `../../HANDOFF-tv-startup.md`：burst 8 约 2s 出 PLAYING、2fps、
`HTTP/1.0` 无 `Content-Length`、`DLNA.ORG_OP=00`。

| 脚本 | 回答的问题 |
|---|---|
| `exp2.py` | 自包含的实验矩阵：开机突发帧数 / 帧大小 / 帧率 / AVI 头字段对出画时间的影响 |

## 线二：服务端出流（频道模型）

背景与成本分析见 `../../CHANNEL.md`。**这条线的门槛性结论还没跑出来**，
见 `CHANNEL.md` 第 9 节。

| 脚本 | 回答的问题 | 状态 |
|---|---|---|
| `ts_server.py` | 服务端能不能实时出一路无限长 H.264+AAC 的 MPEG-TS，电视 HTTP 拉得动 | — |
| `live_ts.py` | 4K 行不行。**已有结论：MJPEG 在 UA60DU7000 上只到 1080p，4K MJPEG 报「格式不支持」**；要 4K 只能走 H.264/HEVC 硬解，而 ESP32 编不动 | 有结论 |
| `swap_server.py` | **同一条 TS 流里中途换素材，电视会不会断流重来。** 判据：换片之后有没有出现新的 GET。`/naive.ts` 是裸拼（时间戳回退、CC 断档），`/remux.ts` 是 concat 流拷贝（时间戳连续） | **结论未记录——这是门槛** |
| `live_switch.py` | 常驻直播 + 运行中切素材的完整做法：素材预切成 2 秒 TS 段，ffmpeg 读一份长 concat 清单，清单每项是符号链接，**读到第 k 段才打开**，所以可以在那一刻前才决定它指向谁。切换延迟 ≈ 1–2 段 | 原型可跑 |
| `push_url.py` | 「服务端出流、板子只做 DLNA 控制点」：不起本地 HTTP 服务，数据通路完全不经过本机 | — |

## 两条已记录的坑

- **断流要让电视自己先停。** 直接断 socket，电视会报「网络中断」——网络其实没断。
  `live_avi.py` 和 `live_ts.py` 收尾处都有这一行注释。
- **`swap_server.py` 的 2 秒段长就是 IDR 间隔**（流拷贝 concat 要求每段从关键帧开始），
  而 IDR 间隔直接决定流量：2 秒 ≈ 155 GB/月/设备，20 秒 ≈ 16 GB。
  见 `CHANNEL.md` 第 5 节。

## 出处

线二这 5 个脚本原先在未跟踪的 `probe/dlna-tv/tools/` 下（2026-10-18 上午），
按 mtime 的推进顺序是 `push_url` → `ts_server` → `swap_server` → `live_switch`，
这个顺序本身就是论证链：先问「板子能不能完全退出数据面」，再问「服务端出流可行吗」，
再问「流内换素材电视受不受得了」，最后才是「怎么换」。
