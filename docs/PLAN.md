# StickBox 电视用量看板 · 项目计划

> 产品方向（2026-10-17 定）：打印桥**整条退役**，设备改为电视信息屏；数据经 Go 服务端下发；
> 配网只用设备自带热点网页，无 App、无蓝牙。界面以 [`docs/design/`](design/) 为准。
>
> 交付形态（2026-10-17 定）：**零售级开源硬件**——按零售成品的工程标准做，以开源硬件交付。
> 取舍、架构决定与里程碑见 [`PRODUCT.md`](PRODUCT.md)。T9 起为产品化任务。


> **工作流更新（2026-10-17 晚，用户决定）**：`main` 即开发主干，`feat/tv-dashboard` 退役。
> - 开发会话（esp / esp1 / 固件会话）写代码，在各自 `task/*` 分支提交后交审计；**只有整合会话（USB-TV）合并进 `main` 并推送**，开发会话不推 `main`、不开/合 PR。
> - 发布与部署由 `v*` tag 把关：只给**上板验收通过**的版本打 tag；正式服务端只部署整合会话审计过的提交。
> - 每个工作会话最多一个 worktree，任务合并后由创建者收回。

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

5. **worktree 只有创建者能删**；集成方只清理「已合并且干净」的。派子 agent 前先向整合报备分工，避免重复实现。

## 任务看板

状态：待办 / 进行中 / 待审计 / 完成

| # | 任务 | 负责 | 依赖 | 状态 |
|---|---|---|---|---|
| T0 | 工程重组：固件移入 `rom/`、删除过时目录、README 与本计划 | 整合 | — | 完成 |
| T1 | 服务端：删除打印功能，补完用量链路 | esp | T0 | 完成 |
| T2 | 电脑端 `stickbox-agent` | esp1 | T7 | 完成 |
| T3 | 固件：删除打印，接入 DLNA 推流引擎与 MQTT 用量 | esp | T0 | 完成 |
| T4 | 固件：按设计稿渲染 1080p 看板（多页轮播） | esp | T3 | **取消**：T13 定为服务端渲染 |
| T5 | 端到端联调、上板与电视验收 | 整合 | T1–T4 | 待办 |
| T6 | 文档收尾：接口文档重写、删除 `probe/`、交接文档 | esp + 整合 | T5 | 完成（T6a 接口文档 + T6b 清理） |
| T7 | 配对码绑定（无手机号）：设备自注册出码，agent 输码配对；删除短信与 App 角色 | esp | T1、T3 | 完成 |
| T8 | 固件内存压缩（T4 动态条带的前提） | esp | T7 | 完成 |
| T9 | OTA 双槽与签名升级通道 | esp（M0 会话） | T7 | 进行中 |
| T10 | 崩溃回传：coredump 上传服务端 | esp（M0 会话） | T9 | 待办 |
| T11 | 服务器地址进 NVS 与配网页 | esp（M0 会话） | T7 | 待审计 |
| T12 | 换 N16R8 板：开 PSRAM、删 BSP 与 LCD、板级引脚抽象 | esp | T9–T11 | **降级为可选**：先用现板（T13） |
| T13 | 渲染位置决策 | 用户 | T8 | **完成：服务端渲染，先用现板** |
| T13a | 服务端渲染器 + 数据源插件（纯 Go） | esp | T13 | 完成 |
| T13c | 服务端帧分发：按用户渲染帧包、GET /api/device/frame、MQTT 通知 | esp1 | T13a | 完成 |
| T13b | 固件取帧：flash 帧缓存环 + 离线兜底帧 | 项目改名 StickBox | T9、T11、T13a | 进行中 |
| T13b-1 | 固件开机取真实画面：读 flash 最新槽先起播，联网后 GET /api/device/frame 写槽 | StickBox | T13c | 进行中 |
| T13b-2 | 固件文字页：正在连接 / 配对码 / 网络离线 / 正在获取数据（构建期 1-bit 掩码，无 demo 图） | StickBox | — | 待审计（7101607） |
| T23 | stickbox-agent doctor 诊断 + 一键安装脚本 + 用户文档 | esp1 | T2、T15b | 待办 |
| T13b-3 | 固件无缝换帧：新帧包校验后在帧边界切换，推流不断、不重推 | StickBox | T13b-1 | 进行中 |
| T15a | 浏览器烧录页（ESP Web Tools 自托管） | esp1 | — | 完成 |
| T15b | 发布流水线：签名固件、镜像、agent 多平台 Release | esp1 | T15a | 暂停（让位 T13c） |
| T17 | 电视首次出画慢（实测约 15 秒）：查三星播放器起播流程，定推流参数 | esp | T13a | 进行中 |
| T18 | 云端下行 MQTT 指令要设备 ACK（cid、应用后回执、退避重投、幂等） | 项目改名 StickBox | T7 | 待办 |
| T14 | 电视兼容矩阵、电视背后 RF 实测、72h soak | 整合 + 用户 | T13 | 待办 |
| T15 | 发布工程：浏览器烧录页、Release、自托管、许可 | esp + 整合 | T14 | 待办 |
| T16 | 开源 PCB 与外壳 | 整合 | T14 | 待办 |

T9–T16 属产品化里程碑 M0–M5，背景与取舍见 [`PRODUCT.md`](PRODUCT.md)。

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
- **接入**：从验证工程搬入 `dlna.*`、`stream.*`、`jband.*`、SNTP 对时（验证工程已在 T6b 删除）；
  订阅 `printer/{dev}/usage`，解析为 `usage_report_t`（payload 格式见 `server/go/internal/usage`）
- 板载 LCD 改为状态屏：Wi-Fi / 电视 / 推流 / 云端 / 最近一次用量时间

**验收**：`idf.py build` 无新增警告；上板后 LCD 显示推流状态，电视出画面（先用预置页即可）；
服务端推一条用量后串口日志能看到解析结果；空闲堆最低值记录在提交说明里（探针时最低 39KB）。

### T4 固件：按设计稿渲染 1080p 看板

> **内存前提已满足（2026-10-17）**：T8 完成后带 TLS 的历史最低为 63.5KB，本任务多占
> 24KB 后预计约 39KB，满足「≥24KB 余量」。板上渲染不再有内存阻塞。
> 是否仍改到服务端做，见 T13 的决策。

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

**接口文档部分（T6a，待审计）**：原 `docs/API-cloud-print.md` 已改名为
[`docs/API.md`](API.md)，按 T7 之后的真实接口重写：设备注册与配对码、MQTT 主题与 ACL、
agent 配对 / 解绑 / 设备 / 用量、错误与限流、部署拓扑；打印、短信、App 内容已全部删除。
`server/go/internal/apidoc/apidoc_test.go` 在 `go test` 中比对文档端点表与
`server/go/internal/httpapi/api.go` 的路由表，防止文档漂移。

**其余部分（T6b，完成）**：删除整个 `probe/`（推流引擎已在 `rom/main`；
实验脚本在 `docs/experiments/tvstart/`；`render_pages.py` 移到 `rom/tools/`）；
`docs/HANDOFF-usb-tv-probe.md` 补 DLNA 结论并引用 `docs/HANDOFF-tv-startup.md`；
`rom/README.md` 补编译、烧录、串口、配网、配对与故障排查。

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

**结果**（2026-10-17，固件 e8f036d，正式服务端 mqtt.stickbox.ai，已配对看板态，连续 9.6 分钟）：

| 项 | T8 前 | T8 后 |
|---|---|---|
| 启动后空闲堆 | 109KB | 148KB |
| 带 MQTT TLS 长连接的历史最低 | 32KB（估算：无 TLS 实测 52KB − 握手 20KB） | **63.5KB**（低点出现在开机 TLS 握手） |
| 稳态空闲 | — | 139~141KB |
| 推流 | 2fps | 2fps（每分钟 120 帧，全程无断流、无重连） |

T4 把条带缓冲从 2×12KB（配对码）换成 2×24KB，只多 24KB：预计最低约 39KB，满足「≥24KB 余量」。

**验收**：真实连上 mqtt.stickbox.ai 的 TLS 长连接 + 推流 + 轮播运行 10 分钟，历史最低空闲 ≥ 48KB + 24KB 余量（即能放下 T4 的条带缓冲后仍有 ≥24KB）；
推流 2fps 不掉；每项改动前后数值写进提交说明。

### T9 OTA 双槽与签名升级通道（M0）

**为什么最先做**：分区表一旦随固件发出去，改它就要每台拆下来接串口重刷，没有补救手段。
现在改只影响手上这几块板。

**分区表**（8MB flash；T12 换 N16R8 后可再扩）：

```
# Name,   Type, SubType,  Offset,   Size
nvs,      data, nvs,      0x9000,   0x6000     # 24K
phy_init, data, phy,      0xf000,   0x1000     # 4K
otadata,  data, ota,      0x10000,  0x2000     # 8K
ota_0,    app,  ota_0,    0x20000,  0x300000   # 3M，app 分区须 64K 对齐
ota_1,    app,  ota_1,    0x320000, 0x300000   # 3M
coredump, data, coredump, 0x620000, 0x10000    # 64K
```

当前固件 2.03MB，3M 槽余量约 48%。0x12000–0x20000 的 56KB 空隙是对齐代价，留作后用。

**签名但不开 secure boot**（理由见 `PRODUCT.md` 第 3 节）：
`CONFIG_SECURE_SIGNED_APPS_NO_SECURE_BOOT=y` + `CONFIG_SECURE_SIGNED_ON_UPDATE_NO_SECURE_BOOT=y`。
私钥 `rom/ota_key.pem` **不入库**（`.gitignore`），公钥编进固件。用户仍可串口刷任意固件。

**回滚**：`CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE=y`。新固件启动后跑健康检查
（Wi-Fi 连上 + MQTT 连上 + 推流起来，或超时 N 秒），通过才 `esp_ota_mark_app_valid_cancel_rollback()`；
否则重启自动回滚上一槽。

**升级通道**：服务端 `GET /api/device/firmware`（设备鉴权）返回
`{version, url, sha256, size}`；设备比对版本号后走 `esp_https_ota`。检查时机：开机一次 + 每天一次。

**验收**：`idf.py build` 过；上板串口能看到当前运行槽位；人为推一版新固件，设备自己升级并切槽，
版本号变化；人为推一版启动即 panic 的固件，验证自动回滚回上一槽且设备恢复服务。

### T10 崩溃回传（M0）

`CONFIG_ESP_COREDUMP_ENABLE_TO_FLASH` 已经开了，缺的是上传——现在只能靠串口
`idf.py coredump-info` 读，别人家里的板子拿不到。

- 开机检查 `esp_core_dump_image_get()`，有镜像就带设备鉴权 `POST /api/device/coredump`
  （原始 ELF，`Content-Type: application/octet-stream`），**上传成功后才** `esp_core_dump_image_erase()`
- 上传失败不擦、不阻塞启动，下次开机重试
- 服务端按 `{dev}/{时间戳}.elf` 存盘，附带固件版本号；`GET /api/agent/coredumps` 列出自己名下的
- 一并上报复位原因 `esp_reset_reason()`，进心跳

**验收**：固件里留一个 `curl` 可触发的 `abort()` 调试端点（Kconfig 开关，默认关），触发后重启，
服务端收到 ELF，本机 `idf.py coredump-info -c <file>` 能解出可读回溯。

### T11 服务器地址进 NVS 与配网页（M0）

**为什么**：现在 `MQTT_URI` / `API_BASE` 是 `cloud_creds.h` 的编译期 `#define`，意味着发不出
一个所有人都能用的预编译 bin，自托管的人必须装 ESP-IDF 自己编译——`PRODUCT.md` 第 6 节的
「浏览器一键烧录」就不成立。

- NVS `cloud` 命名空间加 `mqtt_uri`、`api_base`；读不到时回落到 `cloud_creds.h` 的编译期默认值
  （公共实例），所以预编译 bin 开箱即用、自托管的人改一个字段即可
- 配网页加一个「服务器」输入框，默认填当前生效值，留空表示用默认；放在 Wi-Fi 密码下方，
  标注「自托管才需要改」
- 只接受 `mqtts://` 与 `https://`；格式不合法在网页上报错，不写 NVS
- 长按 MENU 清配置时一并清掉（`provision.c` 的 `nvs_erase_all(NVS_CLOUD)` 已覆盖，确认即可）
- `cloud_creds.h.example` 的注释改写：它现在是**默认值**，不是唯一来源

**验收**：烧一版不含自己服务器地址的固件，仅通过配网页填入地址即可完成配对与收用量；
清配置后地址回到默认值；填入非法地址被网页拒绝。

### T12 换 N16R8 板：开 PSRAM、删 BSP 与 LCD、板级抽象（M1）

目标板见 `PRODUCT.md` 5.1。换板是 T13 服务端渲染的前提（整帧 JPEG 约 120KB，当前
MINI-1-N8 无 PSRAM 放不下；T8 实测内部 RAM 历史最低 32KB）。

- 删 `espressif/esp32_s3_usb_otg` BSP 依赖、`lcd_ui.*`、`main.c` 里 USB-OTG 板专属的 6 个
  GPIO 宏（`PIN_USB_MODE_SEL` / `LIMIT_EN` / `DEV_VBUS_EN` / `BATTERY_BOOST_EN` 和两个 LED）
- 新增 `board.h` 板级引脚抽象，Kconfig 选目标板：通用 WROOM-1 开发板 / 官方 DevKitC-1 / StickBox PCB
- MENU 键 `GPIO_NUM_14` → 板载 BOOT 键 `GPIO_NUM_0`：开机按住清 Wi-Fi 配置
- 状态指示从 LCD 改为板载 WS2812（单灯配色表达 Wi-Fi / 服务端 / 推流三态），配对码只上电视
- 开 PSRAM：`CONFIG_SPIRAM=y`、octal 模式。**八线 PSRAM 占用 GPIO 33–37**，已核对现有固件
  用到的脚（0/12/13/14/15/16/17/18/21）不冲突

**验收**：三个板级目标都能编过；通用 WROOM-1 板上跑通配网 → 配对 → 收用量 → 推流；
`heap_caps_get_free_size(MALLOC_CAP_SPIRAM)` 串口可见且 ≥7MB；WS2812 三态正确。

### T13 渲染位置决策：板上（T4）还是服务端（M2）

> **决策（2026-10-17，用户）：服务端渲染，先用现在的板子（无 PSRAM）顶着。** T4 取消，T12 降级为可选。

**现板上怎么做**：整帧（约 120KB）放不进内部 RAM，所以写进 flash 帧缓存、推流时从 mmap 地址读
（预置页现在就是这么推的，零 RAM）。

- 分区（随 T9 定型，OTA 改不了）：

  | 分区 | 偏移 | 大小 |
  |---|---|---|
  | nvs / phy_init / otadata | 0x9000 / 0xf000 / 0x10000 | 24K / 4K / 8K |
  | ota_0 / ota_1 | 0x20000 / 0x320000 | 3M / 3M（固件 2.07MB） |
  | frames（data 0x40） | 0x620000 | 1.5M = **6 槽 × 256K** |
  | coredump | 0x7a0000 | 64K（末尾 320K 未分配） |

  槽用 256K 不用 128K：1080p 帧约 120KB，128K 槽余量只有 6%，写不下的表现是电视黑屏
- **硬约束：底帧只在数据变化时落 flash（约每 10 分钟），时钟 HH:MM 条带每分钟在内存里用 jband 重编码后拼进推流。**
  整帧每分钟重写 = 每槽每天 240 次 = 寿命约 1.1 年，加槽救不回来，只能少写。按约束写：每槽每天 24 次 ≈ 11 年
- 服务端底帧与设备时钟条带之间的接口约定写在 `docs/FRAME.md`（T13a 起草，T13b 签字）：时钟区 MCU 行范围、
  框外纯色、框内两色、0–9 与冒号的 1 位字形掩码随帧元数据下发、每 MCU 行一个 restart marker
- 帧变化经 MQTT 通知（帧版本号），设备 HTTPS 拉取，带 `If-None-Match`；断网 / 拉取失败时继续推最后一张好帧，
  超过 15 分钟没有新帧切到固件内置的离线兜底帧

#### T13a 服务端渲染器 + 数据源插件（esp）

- 新包 `server/go/internal/render`：按 `docs/design/README.md` 的版面坐标、配色、状态判定、主次自动、计算公式，
  出 1920×1080 baseline JPEG，单帧 ≤ 120KB（超了降质量，不得超过 128KB 槽位）
- 字体：Barlow Condensed / Barlow / Noto Sans SC（OFL），放 `server/go/internal/render/fonts/` 并附 OFL 许可文本
- 新包 `server/go/internal/source`：数据源插件接口（产出结构化数据，渲染器消费）；Claude 用量是第一个实现
- 主机侧对照：`go run ./cmd/render-preview` 把 5 种状态各出一张 JPEG，与 `docs/design/screenshots/` 并排比对
- 冒号闪烁、翻页动画都不做（帧每分钟一换），偏差写进提交说明
- **本任务不接路由、不动 `httpapi` / `config`**（那两处归 T9 会话）；帧分发端点（`GET /api/device/frame` + MQTT 通知）
  在 T9 合并后另开小任务接入

**验收**：5 种状态的渲染图与截图逐区域一致（附并排对照图）；单帧大小、渲染耗时写进提交说明；`go test` 有黄金图回归测试。

**实现（待审计）**：`internal/render` + `internal/source` + `internal/jpegrestart`（支持 DRI 的编码器）
+ `internal/frame`（底帧/图集/索引与拼接）+ `cmd/render-preview`。按 T13b 的方案 D 做 DRI=8
按 MCU 字节拼接：底帧 146,899–184,418 B、图集 28,459 B、索引 4,965 B，最坏合计 217,842 B（256KB 槽）；
逐行时钟列 12 段共 6,030 B；遍历 1440 个时刻的实际最坏 13,089 B（上界 14,626 B）；设备 `BAND_CAP` 16KB（双缓冲 32KB）按行拼接，加载时校验 `max_band_bytes`；
拼接帧与直接渲染整帧逐像素一致，Go 解码器与 `djpeg` 均通过。时钟卡片改为 128×192 并左移，
偏离见 [`docs/FRAME.md`](FRAME.md) 第 6 节，待用户确认。

#### T13b 固件取帧（项目改名 StickBox，T9/T11 之后）

- `frames` 分区环形写：每槽头部写版本号 + 长度 + CRC，开机找最新的有效槽；写一半掉电不能破坏上一张好帧
- 推流从 `esp_partition_mmap` 地址出帧，不占 RAM；离线兜底帧用 EMBED_FILES 打进 app
- **验收**：拔网线 15 分钟内切到离线帧，恢复后自动回到看板；连续 1 小时帧更新不丢；写一半断电（手动拔电）后重启仍显示上一张好帧


> **这是一个待用户拍板的决策，不是一个实现任务。** 定了之后才有实现任务。

**原先搁置 T4 的理由已经失效**：当时的依据是「带 TLS 的内部 RAM 历史最低 32KB，放不下
T4 的 48KB 条带缓冲」。T8 完成后这个数字是 **63.5KB**，T4 多占 24KB 后预计约 39KB，
满足余量要求。**板上渲染现在是可行的**，两条路都成立。

剩下的四条理由仍然站得住（见 `PRODUCT.md` 4.3）：

| | 板上渲染（T4） | 服务端渲染 |
|---|---|---|
| 贡献门槛 | 加数据源要改固件、要有板子、要装 IDF | 服务端写个插件即可 |
| 抗依赖 | `claude -p /usage` 格式一变就要刷固件 | 服务端改一行，设备不动 |
| 工作量 | 字模预渲染 + 弧/环/字形的行渲染 + 按 MCU 行重编码 | Go 里用现成库画，约一天 |
| 字体许可 | 预渲染字模是 OFL 衍生品，要带署名 | 字体只在服务端，无额外义务 |
| 断网 | 本地还能画（时钟仍走） | 要 flash 兜底帧，画面冻结 |
| 带宽 / 服务端成本 | 几乎为零 | 每设备约 20Kbps，服务端要跑渲染 |
| 每秒级动效 | 可以做 | 做不了（帧每分钟换一次） |
| PSRAM | T8 之后不是必需 | 整帧 120KB，需要 T12 换板 |

**定了服务端**：T12 换板是前置，实现任务拆成渲染器（纯 Go，可在 Mac 上对着
`docs/design/screenshots/` 验收，不需要板子）+ 固件取帧与离线兜底。

**定了板上**：直接做 T4，T12 换板降级为可选（仍建议换，为 OTA 槽位与余量）。

- **服务端渲染器**：按 `docs/design/README.md` 的坐标与配色，Go 里出 1920×1080 JPEG。
  5 种状态的输出与 `docs/design/screenshots/` 逐区域比对
- **数据源插件接口**：Claude 用量是第一个实现，不是唯一的。插件产出结构化数据，渲染器消费
- **帧分发**：帧变化时经 MQTT 通知设备，设备 HTTPS 拉取；内容每分钟最多变一次
- **固件侧**：取帧 → 写 flash 帧缓存分区 → 从缓存反复推给电视（MJPEG 要求持续供帧，
  不能只发一次）；flash 写入要做磨损均衡，或整帧常驻 PSRAM 只在掉电时落盘
- **离线兜底**：flash 里预置一张「离线」帧，服务端或网络不可用时显示，不黑屏不花屏
- 冒号每秒闪烁取消（帧率不支持），与设计稿的偏差写进提交说明

**验收**：5 种状态服务端渲染图与截图逐区域一致；上板后电视画面与设计稿一致；拔网线后
15 秒内切到离线帧，恢复后自动回到看板；连续运行 1 小时帧更新不丢。

### T14 电视兼容矩阵、RF 实测、soak（M3）

- **兼容矩阵**：三星 / LG / 索尼 / TCL / 海信 / 小米 / Google TV / Fire TV 近三代机型，
  测 DMR 默认是否开启、SSDP 响应、MJPEG 长流是否超时。产出公开的 `docs/SUPPORTED-TVS.md`
- 顺带验证「已知风险」里那两条：首推 4 秒断流、同网多台电视推给第一个响应者
- **RF**：PCB 天线、设备夹在电视背板与墙之间、路由器在另一个房间——真实客厅测，不在实验室测。
  结果决定自家 PCB 用 WROOM-1 还是 1U + 外置天线
- **soak**：连续 72 小时推流 + 收用量，记录历史最低空闲堆、重连次数、帧更新丢失次数
- **冷启动**：电视关机断 USB 供电是常态，测「电视开机 → 出画面」时间，目标 < 15s

**验收**：矩阵覆盖 ≥6 个品牌并公开；72h soak 无重启、无内存单调下降；冷启动达标。

### T15 发布工程（M4）

- **浏览器烧录页**：GitHub Pages + ESP Web Tools，插 USB 点一下即可刷，不装任何东西。
  依赖 T11（地址进 NVS），否则预编译 bin 没有意义
- GitHub Release 附合并好的 factory bin 与签名；semver + CHANGELOG
- 自托管：`docker-compose up` 起完整后端（已有镜像，补文档与一键配置）
- 许可文件落地：Apache-2.0（代码）、CERN-OHL-S-2.0（硬件）、CC-BY-SA-4.0（文档）
- `CONTRIBUTING.md`、issue 模板、数据源插件开发指南

**验收**：一台没装过 ESP-IDF 的机器，只用浏览器完成烧录 → 配网 → 配对 → 看到画面全程。

### T16 开源 PCB 与外壳（M5）

- KiCad 源 + Gerber + 带 LCSC 料号的 BOM，JLCPCB 可直接下单贴片
- WROOM-1U-N16R8 + 外置天线（依 T14 的 RF 结论定）；USB-A 公头直插电视
- 外壳 STEP + STL，另附「这些现成 USB 外壳装得下」清单
- **不卖组装成品**——理由见 `PRODUCT.md` 第 2 节

**验收**：按公开文件下单一版，装好能跑通全链路。

## 已知风险

- **内存**：已实测结论见 T8——推流 + 轮播 + MQTT TLS 下内部 RAM 历史最低 **32KB**，
  板上渲染要的 48KB 条带缓冲放不下。对策是 T12 换 N16R8（8MB PSRAM）+ T13 服务端渲染，
  不再靠抠内存解决。
- **首推断流**：探针首次推送后电视约 4 秒断开，靠 20 秒后自动重推才播起来（疑为 Stop→SetURI→Play
  里的 Play 打断加载）。T14 的兼容矩阵里逐机型确认。
- **同网多台同名电视**：SSDP 推给第一台回应的，可能不是用户面前那台。需要在配网页里能选电视。T14。
- **数据源是借来的**：`claude -p /usage` 是未公开的输出格式，Anthropic 改一次就全挂。
  对策是 T13 的数据源插件化——用量只是第一个插件，不是唯一的。见 `PRODUCT.md` 4.3。
- **电视背后是 Wi-Fi 最差的位置**：设备夹在金属背板和墙之间。T14 实测，结论决定自家 PCB
  用 PCB 天线还是外置天线。
