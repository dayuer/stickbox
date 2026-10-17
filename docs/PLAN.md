# StickBox 电视用量看板 · 项目计划

> 产品方向（2026-10-17 定）：打印桥**整条退役**，设备改为电视信息屏；数据经 Go 服务端下发；
> 配网只用设备自带热点网页，无 App、无蓝牙。界面以 [`docs/design/`](design/) 为准。

## 分工

| 角色 | 负责 |
|---|---|
| **整合 / 审计**（会话「屏幕翻转显示控制台页面」） | 工程结构、任务拆解、代码审计、上板与端到端测试、合并 `main` |
| **开发**（会话 `esp`） | 按任务实现，自测通过后提交审计 |
| **用户** | 定方向、看电视画面验收 |

## 工作流

1. 每个任务在**自己的分支和 worktree** 上做，从 `feat/tv-dashboard` 拉出：
   `git worktree add -b task/T1-server-cleanup ../esp-airprint-T1 feat/tv-dashboard`
2. 完成后：自测（见各任务验收标准）→ 提交 → 把本文件里该任务的状态改成「待审计」→ 通知审计。
3. 审计通过后由审计合并进 `feat/tv-dashboard`；全部完成再合入 `main`。
4. **不要**在主工作区 `/Users/liyuqing/sproot/esp-airprint` 里开发——那里有用户自己的改动。
5. 板子只有一块：要烧录前先确认没有别的会话在用。烧录后板子停在下载模式要按 RST
   （或 `--after watchdog_reset`）。

## 任务看板

状态：待办 / 进行中 / 待审计 / 完成

| # | 任务 | 负责 | 依赖 | 状态 |
|---|---|---|---|---|
| T0 | 工程重组：固件移入 `rom/`、删除过时目录、README 与本计划 | 整合 | — | 完成 |
| T1 | 服务端：删除打印功能，补完用量链路 | esp | T0 | 待审计 |
| T2 | 电脑端 `stickbox-agent` | esp | T1 | 待办 |
| T3 | 固件：删除打印，接入 DLNA 推流引擎与 MQTT 用量 | esp | T0 | 待办 |
| T4 | 固件：按设计稿渲染 1080p 看板（多页轮播） | esp | T3 | 待办 |
| T5 | 端到端联调、上板与电视验收 | 整合 | T1–T4 | 待办 |
| T6 | 文档收尾：接口文档重写、删除 `probe/`、交接文档 | esp + 整合 | T5 | 待办 |

---

### T1 服务端：删除打印功能，补完用量链路

**删除**（打印整条退役）：作业队列与下发、`raster`、`profile`（打印机档案 / 怪癖）、
适配测试、`janitor`（作业清理）、`printers` / `jobs` / `test_runs` / `profiles` 表、
`device` actor 与 `registry` 里只为作业服务的部分、对应端点与测试、`server/DEPLOY.md`
里与打印和 Python 旧版相关的段落。

**保留**：短信登录与账号、设备 enroll / 解绑 / 认领、密钥与校验缓存、内嵌 MQTT broker
与 ACL、设备心跳（`status`，用于判在线）、用量链路。

**补完用量链路**（已有 WIP 提交 `e78296d`，读它的提交说明）：
- `cmd/stickboxd/main.go` 接上 `br.SetUsageResender(api.Usage())`
- `usage` 包单测：校验边界、Ingest 推给名下所有设备、Resend 补发且 `now` 是现场时间
- 新端点单测：agent 登录（无账号 403）、上报、App 读取、列出 / 吊销 agent、agent 不能访问 app/device 端点
- 集成测试（`integration/`）：真 broker 下设备订阅 `printer/{dev}/usage` → agent 上报 → 设备收到；
  设备重连订阅时收到补发
- ACL 矩阵测试已覆盖 usage 与 agent，删除打印后保持全绿

**验收**：`go vet ./... && go test ./...` 全绿；`grep -ri "job\|raster\|printer" server/go` 只剩
确有理由保留的（例如 topic 前缀 `printer/` 若保留需在文档说明原因）。

### T2 电脑端 `stickbox-agent`

放在 `server/go/cmd/stickbox-agent`，Go 单文件程序，macOS / Linux / Windows：
- `login`：手机号 + 短信验证码 → `POST /api/agent/login` → 令牌存用户配置目录（0600）
- `run`：常驻，每 10 分钟执行
  `claude -p /usage --setting-sources project --no-session-persistence --output-format stream-json --verbose`，
  解析 `usage_report.rate_limits.limits`（`session` → five_hour，`weekly_all` → seven_day，原始 limits 一并上报），
  `POST /api/usage`
- `install` / `uninstall`：macOS launchd、Linux systemd --user、Windows 计划任务
- `status`、`logout`

**必须处理的坑**（实测过）：
- 环境里的 `ANTHROPIC_*` / `CLAUDE_*` 变量会把 claude 指向别处或注入宿主凭据，子进程环境要剔掉
- 用户 settings.json 可能把 CLI 指向第三方接口，所以必须 `--setting-sources project`
- 用量接口 429 后客户端本地冷却约 1 小时，期间 `rate_limits` 为 null——跳过本轮，不报错退出
- launchd / systemd 下 PATH 很短，`install` 时解析出 claude 的绝对路径写进配置
- `resets_at` 是 ISO 8601（可能带小数秒和 `+00:00`）

**验收**：解析逻辑有单测（用 `rom/tools/fixtures/usage-2026-10-17.json` 一类的脱敏样例）；
在本机对测试服务端跑通 login → run 一轮 → 服务端 `GET /api/usage` 能读到。

### T3 固件：删除打印，接入推流引擎与 MQTT 用量

基础是 `rom/`（原打印桥固件）。
- **删除**：`usb_printer.*`、`usb_quirks_db.h`、`printer_profile.*`、`profile_script.*`、`joblog.*`、
  `usage_http.*`（局域网直推，已废弃）、`cloud_client.c` 里作业 / 取件 / 档案 / 探针命令相关代码、
  USB host 切换（`board_usb_host_power`）
- **保留**：热点配网门户 `provision.*`、设备密钥、MQTT 长连接（心跳照发，判在线用）、
  LCD（`lcd_ui.*`，180° 翻转）、netlog
- **接入**：从 `probe/dlna-tv/` 搬入 `dlna.*`、`stream.*`、`jband.*`、SNTP 对时；
  订阅 `printer/{dev}/usage`，解析为 `usage_report_t`（payload 格式见 `server/go/internal/usage`）
- 板载 LCD 改为状态屏：Wi-Fi / 电视 / 推流 / 云端 / 最近一次用量时间

**验收**：`idf.py build` 无新增警告；上板后 LCD 显示推流状态，电视出画面（先用预置页即可）；
服务端推一条用量后串口日志能看到解析结果；空闲堆最低值记录在提交说明里（探针时最低 39KB）。

### T4 固件：按设计稿渲染 1080p 看板

规格以 `docs/design/README.md` 为准（版面坐标、配色、状态判定、主次自动、计算公式都在里面）。
- 渲染器：把 `jband` 从「单色 shader」扩展为按像素出 RGB 的行渲染（矩形、细线框、「+」角标、
  弧 / 环、字形贴图），按 MCU 行只重编码变化的条带
- 字形：Barlow Condensed / Barlow / Noto Sans SC（均 OFL）按需要的字号和字符集**预渲染**成
  灰度字模放进 flash（构建脚本放 `rom/tools/`），不在板上跑矢量字体
- 动态区域刷新：时钟 HH:MM（冒号每秒闪）、不足 1 天时次窗口剩余时间的秒、「更新 X分前」；
  数据变化时整帧重编码
- 翻页时钟的翻转动画：MJPEG 每秒约 2 帧，**不做动画**，数字直接切换（与设计稿的偏差，需在提交说明里写明）
- 多页轮播：第 1 页为设计稿看板；其余页面待定，先留接口
- 主机侧渲染测试：Mac 上用同一份渲染代码出 JPEG，与 `docs/design/screenshots/` 的 5 种状态并排比对

**验收**：5 种状态的主机侧渲染图与截图逐区域一致；上板后电视画面与设计稿一致，时钟每分钟准确跳变，
重编码耗时与空闲堆最低值写进提交说明。

### T5 端到端联调（整合）

agent 上报 → 服务端 → MQTT → 板子 → 电视，全链路；断网 / 电视关机 / 服务端重启 / 板子重启各恢复一次。

### T6 文档收尾

`docs/API-cloud-print.md` 改写为只含账号、设备、MQTT、用量与 agent 的接口文档（改名）；
`docs/HANDOFF-usb-tv-probe.md` 补 DLNA 结论；删除 `probe/`；`rom/README.md` 写烧录与调试要点。

## 已知风险

- **内存**：探针单独推流时空闲堆最低 39KB；T3 还要加 MQTT 的 TLS 长连接（约 20~30KB）。
  打印取件那条 HTTPS 连接（约 42KB）已随打印删除省掉，账面够，但必须实测。
- **首推断流**：探针首次推送后电视约 4 秒断开，靠 20 秒后自动重推才播起来（疑为 Stop→SetURI→Play
  里的 Play 打断加载）。T3 顺手查。
- **同网多台同名电视**：SSDP 推给第一台回应的，可能不是用户面前那台。需要在配网页或控制面板里能选电视。
