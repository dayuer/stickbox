# 电视兼容性矩阵

StickBox 通过 DLNA/UPnP 把 1080p MJPEG/AVI 直播流推给电视。不同品牌、甚至同品牌
不同固件对 MJPEG 长流、DLNA 头和 SOAP 的脾气并不一样。这张表记录**实测过**的机型；
买板子之前，请先用 `stickbox-tvcheck` 测一下你的电视，把结果提交上来。

> 通过标准（建议）：`Play` 后 ≤3 秒进入 PLAYING，并且持续拉流 3 分钟不断开。
> 只要能播但出现断流/起播慢，也欢迎提交——我们按机型修参数。

## 矩阵

| 电视 | 地区/年份 | DLNA DMR | SetURI / Play | 起播（burst 8 / 2fps） | 3 分钟长跑 | 电视请求特征 | 已知问题 | 测试 |
|---|---|---|---|---|---|---|---|---|
| 三星 UA60DU7000KXXD（Room C，192.168.3.18） | 2024 / 中国 | 是，AVTransport `:9197` | 200 / 200 | 1.1 s（TRANSITIONING 1.0 s） | 通过：180 s 实测 1 次 GET、0 断流（T17 另有 6 分钟无断流） | HEAD 后 GET；无 User-Agent、无 Range。HEAD 带 `getcontentFeatures.dlna.org`、`getCaptionInfo.sec`；GET 带 `transferMode.dlna.org: Streaming`、`getMediaInfo.sec`，不要求 Content-Length | ① Play 后先 `TRANSITIONING` 1–2.5 s，正常；② 曾被数字实体（`&#34;`）打成 402，T17 起只转义 `& < >`；③ 首推偶发约 4 s 断流，burst 8 缓解；④ GetProtocolInfo 属于 ConnectionManager，tvcheck 首次误发 AVTransport 得到 500，已修复（待复测 MIME） | 2026-10-17，USB-TV（T14a 实测）/ T17 |

> 表中“起播”是 `Play` 返回到 `GetTransportInfo=PLAYING` 的时间；`GET` 是电视开始拉流的时刻。
> 实际出画时刻会比 PLAYING 再晚一点，目前没有逐机型记录。

Room C 的 T14a 实测（2026-10-17，`stickbox-tvcheck --minutes 3`）：

- SetAVTransportURI / Play：200 / 200
- 进入 PLAYING：1.1 s；其中 TRANSITIONING 1.0 s
- 180 s 内：1 次 GET、0 次断开；电视先 HEAD 再 GET，无 User-Agent、无 Range
- 已知问题：GetProtocolInfo 被工具误发到 AVTransport（HTTP 500），已改为从设备描述里
  找 ConnectionManager 的 controlURL；Room C 的 MIME 声明待复测后补上

### 怎么读这张表

- **DLNA DMR = 否**：电视不提供 AVTransport，StickBox 无法推送。
- **SetURI / Play 非 200**：SOAP 层就被拒；常见是 402（参数/实体问题）或 701（状态机不允许）。
- **起播 > 5 s**：可以看，但体验差；把报告发上来，我们试 burst/AVI 头变体。
- **长跑断开**：记录断开时刻、重连耗时和电视请求头；首推断开和“缓冲填满后主动断开”是两类问题。

## 用 `stickbox-tvcheck` 测你的电视

`stickbox-tvcheck` 在电脑上运行，不需要 StickBox 板子。

```sh
cd server/go
go run ./cmd/stickbox-tvcheck
```

或者构建成二进制：

```sh
go build -o stickbox-tvcheck ./cmd/stickbox-tvcheck
./stickbox-tvcheck
```

步骤：

1. 电视开机，和电脑连同一个局域网；确认电视的 DLNA/投屏功能已开启
   （三星：设置 → 通用 → 外部设备管理器 → 设备连接管理；其他品牌类似）。
2. 运行命令，工具会用 SSDP 列出局域网里的 DLNA 渲染器，选择你的电视
   （也可以 `--device <编号|IP|名称>` 直接指定）。
3. 电视上会出现一张测试卡（时间每秒跳动）。**保持 3 分钟，不要切台/待机**。
   电视可能会弹“允许投屏”之类的确认框，请允许。
4. 结束后得到 `stickbox-tvcheck.json` 和 `stickbox-tvcheck.md`。把 Markdown
   贴到 [GitHub issue](https://github.com/dayuer/stickbox/issues)（选“电视兼容性报告”），
   并附上 JSON 文件。

常用参数：

```sh
stickbox-tvcheck --minutes 5          # 长跑 5 分钟
stickbox-tvcheck --device 2           # 直接选第 2 台
stickbox-tvcheck --device 192.168.3.18
stickbox-tvcheck --fps 2 --burst 8    # 与固件一致（默认）
```

### 工具会记录什么

- SSDP 发现的名称、厂商、型号、控制地址；
- `GetProtocolInfo` 里声明的视频 MIME；
- `Stop` / `SetAVTransportURI` / `Play` 的真实 HTTP 状态码；
- 从 `Play` 到 `PLAYING` 的耗时，以及 `TRANSITIONING` 持续多久；
- 持续拉流期间的连接次数、断开次数、每次断到重连的耗时；
- 电视发来的 HTTP 请求：方法（HEAD/GET）、`User-Agent`、是否带 `Range`、DLNA 相关头；
- 已知坑的自动提示：402/数字实体、TRANSITIONING 过慢、首推断开、未声明 `video/avi` 等。

## 提交新机型

Issue 里请包含：

1. 电视品牌、完整型号、购买年份/地区、固件版本（设置 → 关于本机）；
2. 电视是 Wi-Fi 还是有线，中间有没有交换机/路由器的特殊设置；
3. `stickbox-tvcheck.md` 的内容（直接粘贴）和 `stickbox-tvcheck.json` 附件；
4. 屏幕上的实际出画时间（可选，用手机秒表；PLAYING 不等于画面出来）。

测完后我们会把结果补进上表；如果命中新的坑，会同步到固件/工具的参数里。

## 已知问题索引

| 现象 | 原因 | 当前处理 |
|---|---|---|
| `402 Invalid Args` | 三星 Room C 的 SOAP 解析器不认 `&#34;` 等数字实体 | T17 起 SOAP 只转义 `& < >`；tvcheck 会直接提示 |
| Play 后先 `TRANSITIONING` 数秒 | 电视起播状态机，正常现象 | 属于正常；超过 5 s 才标记为慢 |
| 首推约 4 秒断开 | 疑似 Stop→SetURI→Play 打断加载 | 固件起播连发 burst 8 帧；tvcheck 会记录首次断开时刻 |
| `GetProtocolInfo` 没有 `video/avi` | 部分电视声明不全，但仍能播 MJPEG | 报告里保留 MIME 列表，不直接判失败 |
| HEAD 探测 | 部分电视先 HEAD 再 GET | 服务端对 HEAD 只回头不回帧，正常 |
| 请求带 Range | 少数播放器会带 | 直播流按 DLNA Streaming 不返回 Content-Length，Range 被忽略 |
