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
| T1 | 服务端：删除打印功能，补完用量链路 | esp | T0 | 完成 |
| T2 | 电脑端 `stickbox-agent` | esp | T7 | 待办 |
| T3 | 固件：删除打印，接入 DLNA 推流引擎与 MQTT 用量 | esp | T0 | 完成 |
| T4 | 固件：按设计稿渲染 1080p 看板（多页轮播） | esp | T3 | 待办 |
| T5 | 端到端联调、上板与电视验收 | 整合 | T1–T4 | 待办 |
| T6 | 文档收尾：接口文档重写、删除 `probe/`、交接文档 | esp + 整合 | T5 | 待办 |
| T8 | 固件内存压缩（T4 动态条带的前提） | esp | T7 | 待办 |
| T7 | 配对码绑定（无手机号）：设备自注册出码，agent 输码配对；删除短信与 App 角色 | esp | T1、T3 | 待办 |

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
- `pair <配对码>`：`POST /api/agent/pair`（已有令牌就带上，把新屏并入同一用户）→ 令牌存用户配置目录（0600）；
  `unpair <dev>`、`devices` 列出名下屏（见 T7）
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
在本机对测试服务端跑通 pair → run 一轮 → 服务端 `GET /api/agent/usage` 能读到。

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

### T7 配对码绑定（无手机号、无短信）

配网页**只配 Wi-Fi**。账号与设备的关联靠电视 / LCD 上显示的配对码，在电脑上输入：

```
设备联网 → POST /api/device/register {dev} → 拿到设备密钥 + 6 位配对码 → 电视和 LCD 显示配对码
用户电脑 stickbox-agent pair 123456 → POST /api/agent/pair → 设备归属该 agent 的用户，设备收到「已配对」切到看板
```

**服务端**
- `POST /api/device/register {dev}`（不鉴权）：
  - MAC 未注册：建设备记录，签发设备密钥，生成配对码，返回 `{token, code, expires_at}`
  - MAC 已绑定（设备被擦了 NVS 重来）：**不吊销**旧密钥；新密钥处于「待配对」，只有原主人的 agent 输码配对才转正
    （同时吊销旧密钥），其他人输码返回 409。否则知道 MAC 就能把别人的屏顶掉
  - 按 dev 与来源 IP 限流
- `POST /api/device/pair-code`（设备鉴权）：码过期后设备换新码
- 配对码：6 位数字、10 分钟有效、一次性；`/api/agent/pair` 按来源 IP 限失败次数（如 10 分钟 10 次），
  并有全局失败速率上限，防穷举
- `POST /api/agent/pair {code, name}`：
  - 不带令牌：新建一个无手机号的用户，设备归它，签发 agent 令牌，返回 `{token, dev}`
  - 带 agent 令牌：设备并入该用户（一个 agent 管多台屏）
- 配对成功后经 MQTT 通知设备（主题自定，ACL 只许设备读自己的），并立即补发一份最新用量
- 解绑：`POST /api/agent/unpair {dev}`（agent 鉴权，只能解自己名下的）；解绑后设备回到出码状态
- **删除**（没有 App、没有手机号，就没有使用者了）：短信模块与 `/api/auth/sms`、`/api/auth/verify`、
  `/api/agent/login`、app 角色及其全部端点（`/api/devices`、enroll / unbind / 待认领密钥、`GET /api/usage`、
  `GET /api/agents`、吊销端点）、`phone_*` 配置与用户表里的手机号字段；需要的查询改成 agent 鉴权
  （`GET /api/agent/devices`、`GET /api/agent/usage`）
- 单测：注册、出码、配对新用户、配对并入已有用户、过期码、一次性、穷举限流、已绑 MAC 重注册（原主人转正 / 他人 409）、
  解绑、ACL；integration：注册 → 配对 → 设备经 MQTT 收到已配对和用量

**固件**
- 配网页只保留 Wi-Fi；门户里与 enroll 密钥相关的字段（`k`）删除
- 联网后没有密钥就 register，密钥写 NVS；未配对时 LCD 与电视显示配对码和一句说明
  （电视用 T3 的预置页即可，T4 再按设计稿做配对页），码过期自动换
- 收到已配对后切到看板；收到解绑回到出码

**验收**：`go test ./...` 全绿；上板：清配置 → 配 Wi-Fi → 电视和 LCD 出码 → 本机 `curl` 调 `/api/agent/pair` 配对 →
设备切看板并收到用量；擦 NVS 重来时原主人能转正、换一个 agent 配对被拒。

### T8 固件内存压缩

**实测**（2026-10-17，feat/tv-dashboard cbc9021 + 临时探针，推流 + 三页轮播运行 45 秒）：

| 项 | 数值 |
|---|---|
| 内部 RAM 空闲 / 历史最低 | 87KB / 52KB |
| TLS 连接（同一套 mbedtls 配置，GET https 一次） | 握手峰值约 45KB，连接保持时常驻约 42KB |
| 加上 TLS 后历史最低 | **32KB**；T4 还要两块 24KB 条带缓冲（48KB）——**放不下** |
| LVGL 内置堆（静态 .bss） | 64KB，峰值只用 9.6KB |
| 任务栈余量最低 | taskLVGL 3.3K、mqtt_task 3.4K、esp_timer 3.1K、dlna_sup 3.0K、lcd_ui 1.9K、cycle 1.8K、avi_srv 1.8K |

**要做**（按收益排序）：
1. `CONFIG_LV_MEM_SIZE_KILOBYTES` 64 → 16（实测峰值 9.6KB，留余量；配对码页面加上后重测）——省约 48KB
2. `CONFIG_MBEDTLS_SSL_IN_CONTENT_LEN` 16384 → 8192（服务端只有 MQTT 小消息，证书链约 3~4KB；需对 mqtt.stickbox.ai 实测握手）——握手峰值省约 8KB
3. 任务栈按余量收缩，每个留 ≥1KB：taskLVGL、mqtt_task（`CONFIG_MQTT_TASK_STACK_SIZE`）、esp_timer、dlna_sup——合计约 8KB
4. MQTT 收发缓冲（`CONFIG_MQTT_BUFFER_SIZE`）按用量 payload 大小定，不用默认值
5. Wi-Fi 动态 TX 缓冲 16 → 10，测推流帧率不降

**验收**：真实连上 mqtt.stickbox.ai 的 TLS 长连接 + 推流 + 轮播运行 10 分钟，历史最低空闲 ≥ 48KB + 24KB 余量（即能放下 T4 的条带缓冲后仍有 ≥24KB）；
推流 2fps 不掉；每项改动前后数值写进提交说明。

## 已知风险

- **内存**：探针单独推流时空闲堆最低 39KB；T3 还要加 MQTT 的 TLS 长连接（约 20~30KB）。
  打印取件那条 HTTPS 连接（约 42KB）已随打印删除省掉，账面够，但必须实测。
- **首推断流**：探针首次推送后电视约 4 秒断开，靠 20 秒后自动重推才播起来（疑为 Stop→SetURI→Play
  里的 Play 打断加载）。T3 顺手查。
- **同网多台同名电视**：SSDP 推给第一台回应的，可能不是用户面前那台。需要在配网页或控制面板里能选电视。
