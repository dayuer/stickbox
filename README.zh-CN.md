<div align="center">

# StickBox

**一块插在电视 USB 口上的 ESP32-S3，把任何电视变成 Claude 用量看板。**

[![License: MIT](https://img.shields.io/github/license/dayuer/stickbox)](LICENSE)
[![Latest release](https://img.shields.io/github/v/release/dayuer/stickbox)](https://github.com/dayuer/stickbox/releases/latest)
![ESP-IDF 5.5](https://img.shields.io/badge/ESP--IDF-5.5-red)
![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8)

[English](README.md) · 简体中文

<img src="docs/design/screenshots/02-%E5%AE%89%E5%85%A8.png" alt="电视上的 1080p 用量看板" width="720">

<sub>1080p 看板的设计渲染图。</sub>

</div>

StickBox 把你的 **Claude 订阅用量**——5 小时和 7 天两个额度窗口用了多少、按当前速度消耗得多快、
会不会在重置前用完——放大成房间里一眼就能看清的电视看板。板子只从电视 USB 口取电，画面走家里的 Wi-Fi。

## 特性

- **电视上的用量看板**：5 小时 / 7 天额度、时间进度、消耗节奏与建议上限，1080p 渲染，带翻页时钟。
- **不要 App，不要手机号，不把账号交给别人的云**：电视上显示 6 位配对码，配对后在电脑上跑一个小 agent。
- **Claude 凭据不出你的电脑**：agent 在本机执行 `claude -p /usage`（不消耗额度），只上传数字。
- **无缝 1080p 的 DLNA 推流**：板子用 SSDP 找到电视，推一路「永不结束」的 MJPEG 流，换内容只是换帧，
  没有工具条，也不会闪回首页。
- **没有 PSRAM 也能出 1080p**：整帧切成可独立解码的 JPEG 片段（restart marker），只重编码变化的那几条。
- **照片轮播（可选）**：插上 SD 卡，用 agent 上传的照片会穿插在看板之间播放。
- **带签名的 OTA 升级**：RSA-3072 签名固件，A/B 双槽，失败自动回滚。
- **可自托管**：服务端是一个 Go 二进制加一个 SQLite 文件，自带 MQTT broker。

## 工作原理

```
你的电脑                   服务端（Go）               ESP32-S3 板子              电视
┌────────────────┐  HTTPS  ┌────────────────┐  MQTT   ┌─────────────────┐  DLNA  ┌──────┐
│ stickbox-agent │ ──────▶ │   stickboxd    │ ──────▶ │ 渲染 + 推流     │ ─────▶ │ 1080p│
│ claude /usage  │ 令牌    │ 存最新用量并   │  TLS    │ HTTP 上的 MJPEG │  SSDP  │ MJPEG│
└────────────────┘         │ 渲染看板底帧   │         └─────────────────┘        └──────┘
                           └────────────────┘
```

Anthropic 没有开放订阅用量的第三方授权，所以 agent 用的是**你自己**本机的 Claude 登录。服务端只给
agent 签发权限受限的令牌，永远接触不到你的 Claude 凭据。详见 [`docs/AGENT.md`](docs/AGENT.md)、
[`docs/SECURITY.md`](docs/SECURITY.md)。

## 硬件与电视兼容性

| | 已测试 |
|---|---|
| 板子 | Espressif **ESP32-S3-USB-OTG**（USB 取电、240×240 状态屏、可选 SD 卡槽） |
| 电视 | 三星 UA60DU7000（2024）——DLNA MJPEG 1080p，3 分钟长跑无断流 |

不同品牌、甚至同品牌不同固件的电视对 DLNA 的脾气不一样。买板子之前，请先用兼容性检测工具测一下你的电视，
也欢迎把报告提交给我们：

```bash
cd server/go && go run ./cmd/stickbox-tvcheck --help
```

兼容性矩阵见 [`docs/SUPPORTED-TVS.md`](docs/SUPPORTED-TVS.md)。

## 快速上手

你需要：一块受支持的板子、同一网络里支持 DLNA 的电视，以及在电脑上已登录的
[Claude Code](https://claude.com/claude-code) 命令行。

### 1. 刷固件

从[最新发布](https://github.com/dayuer/stickbox/releases/latest)下载 `stickbox-firmware.zip`，按其中
`flasher_args.json` 列出的偏移用 `esptool` 刷入。仓库里还有一个静态的浏览器烧录页
（ESP Web Tools，Chrome 或 Edge）在 [`web/flash/`](web/flash/)，用
[`rom/tools/gen_web_manifest.py`](rom/tools/gen_web_manifest.py) 生成它的 manifest 后自行托管。

首次上电，板子会开一个 `StickBox-Setup-XXXX` 热点，手机连上后自动弹出配网页。开机按住 MENU 键（GPIO14）
可清空 Wi-Fi 配置。

> 官方固件连的是作者托管的服务器。想用自己的服务器，请自行编译固件并填你的地址，见[自托管](#自托管)。

### 2. 安装 agent

从发布页下载对应平台（macOS、Linux、Windows；amd64 或 arm64）的二进制，用 `SHA256SUMS` 校验后：

```bash
stickbox-agent pair 123456   # 电视上显示的 6 位配对码
stickbox-agent run --once    # 上报一轮，确认链路通了
stickbox-agent install       # 登录后在后台常驻（不需要 sudo）
stickbox-agent status        # 服务器、配对、上次成功、claude 登录状态
stickbox-agent doctor        # 自检
```

可选命令：`tz set Asia/Jakarta` 设置时区，`photo add / ls / rm / init` 管理 SD 卡照片轮播。
完整说明见 [`docs/AGENT.md`](docs/AGENT.md)。

## 自托管

服务端是一个 Go 二进制（`stickboxd`）加一个 SQLite 文件。

```bash
cd server/go
go test ./... && go build ./cmd/stickboxd
```

部署、TLS、备份、升级见 [`server/DEPLOY.md`](server/DEPLOY.md) 和 [`docs/OPS.md`](docs/OPS.md)。
让固件连你的服务器：

```bash
source ~/esp/esp-idf-v5.5/export.sh
cd rom
cp main/cloud_creds.h.example main/cloud_creds.h   # 填你的服务器地址
idf.py build
idf.py -p /dev/cu.usbmodemXXXX flash
```

官方发布用的签名私钥由维护者保管；你自己编译固件时需要生成自己的签名密钥，见 [`docs/RELEASE.md`](docs/RELEASE.md)。

## 目录结构

| 路径 | 说明 |
|---|---|
| [`rom/`](rom/) | ESP32-S3 固件（ESP-IDF 5.5）：配网、MQTT、DLNA 推流、渲染、LCD 状态屏、OTA |
| [`server/go/`](server/go/) | `stickboxd` 服务端、`stickbox-agent`、`stickbox-tvcheck` 等工具 |
| [`web/flash/`](web/flash/) | 浏览器烧录页 |
| [`docs/`](docs/) | 设计、协议与运维文档 |
| [`probe/`](probe/) | 早期 DLNA / 电视实验（仅作参考） |

## 文档

| 主题 | 文档 |
|---|---|
| HTTP / MQTT 接口 | [`docs/API.md`](docs/API.md) |
| 看板帧协议 | [`docs/FRAME.md`](docs/FRAME.md) |
| 决策与约束 | [`docs/DECISIONS.md`](docs/DECISIONS.md) |
| 照片轮播 | [`docs/PHOTO-USER.md`](docs/PHOTO-USER.md)、[`docs/PHOTO.md`](docs/PHOTO.md) |
| 上板验收 | [`docs/BOARD-ACCEPTANCE.md`](docs/BOARD-ACCEPTANCE.md) |
| 发布流程 | [`docs/RELEASE.md`](docs/RELEASE.md) |
| 安全模型 | [`docs/SECURITY.md`](docs/SECURITY.md) |
| 设计稿 | [`docs/design/`](docs/design/) |

## 项目状态

当前版本：**v1.1.x**。范围是 ESP32-S3 板子上的 Claude 用量看板，项目处于维护模式：欢迎修缺陷，大型新功能不在范围内。

## 参与贡献

欢迎提 Issue 和 Pull Request。

- 大改动请先开 Issue 讨论。硬件问题请附上板子、电视型号和固件版本；agent 问题请附 `stickbox-agent status` 输出。
- 服务端和 agent 改动需通过 `cd server/go && go vet ./... && go test ./... -race`。
- 固件改动需通过 `rom/tools/bandtest/run.sh`，并在真实板子上验证；请写明用的板子和电视。
- 固件请在干净导出（`git archive`）的目录里编译，不要在含有你本地 `cloud_creds.h` 的工作区里编译。
- 不要提交任何密钥（`cloud_creds.h`、签名私钥、令牌）。

## 安全

发现漏洞请不要公开提 Issue，请通过 GitHub 私下联系维护者。令牌模型见 [`docs/SECURITY.md`](docs/SECURITY.md)。

## 许可证

[MIT](LICENSE) © 2026 dayuer。

第三方组件保留各自的许可证，例如 ESP-IDF（Apache-2.0）、LVGL（MIT）以及
[`server/go/go.mod`](server/go/go.mod) 中列出的 Go 模块。固件内嵌的中文点阵字体
（`rom/main/font_cn_16.c`）由第三方字体生成；如果你要商业再分发，请改用你已核实许可证的字体
（例如 OFL 授权的 Noto Sans SC）重新生成。
