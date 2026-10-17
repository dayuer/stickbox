# StickBox

把一块 ESP32-S3 插在电视的 USB 口上取电，让电视变成一块 **Claude 订阅用量看板**：
5 小时 / 7 天两个额度窗口用了多少、按当前速度会不会提前用完，房间里任何位置一眼看清。

```
用户电脑 stickbox-agent ──agent 令牌──▶ stickboxd（Go）──MQTT──▶ ESP32 ──DLNA 1080p MJPEG──▶ 电视
   用用户自己的 Claude 登录                 存最新用量           收用量、板上渲染
   跑 claude -p /usage                     推给名下设备          推流给电视
```

- **数据从哪来**：Anthropic 没有开放订阅用量的第三方授权。用户电脑上的 `stickbox-agent`
  用**用户自己的** Claude 登录执行 `claude -p /usage`（本地命令，不耗额度），带服务端签发的
  agent 令牌上报。服务端从不接触 Claude 凭据。
- **怎么上电视**：板子用 SSDP 找到电视、DLNA 推送一路「永不结束」的 1080p MJPEG 流，
  换内容只是换帧——无工具条、无闪回首页。USB 只负责供电。
- **怎么在没 PSRAM 的板子上出 1080p**：画面按 16 像素行切成可独立解码的 JPEG 片段
  （restart marker），只重编码变化的那几条。

## 目录

| 目录 | 是什么 | 说明 |
|---|---|---|
| [`rom/`](rom/) | ESP32-S3 固件（ESP-IDF 5.5） | 配网、MQTT、推流、看板渲染、板载 LCD 状态屏 |
| [`server/`](server/) | Go 服务端 `stickboxd` 与电脑端 `stickbox-agent` | 账号、设备绑定、内嵌 MQTT broker、用量链路 |
| [`docs/`](docs/) | 设计稿、接口文档、计划 | 见下 |

## 文档

- [`docs/PLAN.md`](docs/PLAN.md) — **项目计划与任务看板**（先看这个）
- [`docs/design/`](docs/design/) — 1080p 电视看板高保真设计稿（规格以其 README 为准）
- [`docs/API.md`](docs/API.md) — HTTP / MQTT 接口：设备注册与配对码、agent 配对、用量上报与推送
- [`docs/FRAME.md`](docs/FRAME.md) — 服务端看板帧接口：底帧、字形图集、MCU 拼接
- [`docs/HANDOFF-tv-startup.md`](docs/HANDOFF-tv-startup.md) — 电视起播延迟实验与推流参数
- [`docs/HANDOFF-usb-tv-probe.md`](docs/HANDOFF-usb-tv-probe.md) — 电视接入的实验记录（U 盘方案为何放弃、三星电视能力）
- [`server/DEPLOY.md`](server/DEPLOY.md) — 服务端部署
- [`docs/OPS.md`](docs/OPS.md) — 运维：升级/回滚、证书、备份、指标与告警
- [`docs/experiments/tvstart/`](docs/experiments/tvstart/) — DLNA 推流/起播实验脚本

## 快速上手

**固件**

```bash
source ~/esp/esp-idf-v5.5/export.sh
cd rom
cp main/cloud_creds.h.example main/cloud_creds.h   # 填服务器地址
idf.py build
idf.py -p /dev/cu.usbmodemXXXX flash
```

首次上电开 `StickBox-Setup-XXXX` 热点，手机连上后自动弹出配网页（设备自带的网页，不需要 App）。
开机按住 MENU 键（GPIO14）清空 Wi-Fi 配置。

**服务端**

```bash
cd server/go && go test ./... && go build ./cmd/stickboxd
```

部署见 [`server/DEPLOY.md`](server/DEPLOY.md)。

## 协作方式

- `main` 只接受审计过的合并。每个任务一个分支 / worktree，完成后提交审计。
- 任务、负责人、验收标准、状态都在 [`docs/PLAN.md`](docs/PLAN.md)；做完一项就更新它。
- 固件改动必须上板验证；服务端改动 `go test ./...` 全绿才算完。
