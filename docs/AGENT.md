# stickbox-agent 使用说明

`stickbox-agent` 装在**你自己的电脑**上。它每隔 10 分钟用你本机的 Claude Code
执行一次 `/usage`，把五小时 / 七天用量上报给 StickBox 服务端，服务端再推给你名下
的屏幕（电视、LCD）。它不读取你的对话内容，只读用量百分比和重置时间。

支持 macOS 和 Linux（Windows 可以手动下载二进制，见文末）。

---

## 1. 安装

前提：

- 已安装 Claude Code，并且**已经登录**（终端里 `claude` 能正常对话）；
- 能访问 StickBox 服务端（默认 `https://mqtt.stickbox.ai`）。

一键安装（macOS / Linux，不需要 sudo）：

```sh
curl -fsSL https://raw.githubusercontent.com/dayuer/stickbox/main/server/go/cmd/stickbox-agent/install.sh | sh
```

或者下载仓库里的 `server/go/cmd/stickbox-agent/install.sh` 后：

```sh
sh install.sh
```

脚本会从 GitHub Release 下载对应平台的 `stickbox-agent`，按同一 Release 的
`SHA256SUMS` 校验后放进 `~/.local/bin`。如果这个目录不在 `PATH` 里，脚本会提示
你加一行 `export PATH="$HOME/.local/bin:$PATH"`。

指定版本或安装目录：

```sh
sh install.sh --version v0.9.5 --dir "$HOME/bin"
```

> 只下载二进制，不会自动配对，也不会自动安装后台服务——这两步需要你确认。

---

## 2. 配对

电视或屏幕上会显示 6 位配对码。在电脑上运行：

```sh
stickbox-agent pair 123456
```

- 配对本机 Claude 用量和这块屏幕；一个 agent 可以管多块屏，把每块屏的码依次
  `pair` 一次即可。
- 令牌保存在用户配置目录的 `agent.json`（权限 0600），服务端只存哈希。
- 自托管服务端：

  ```sh
  stickbox-agent pair --server https://你的域名 123456
  ```

  自签证书再加 `--insecure`（会关闭 TLS 校验，仅限可信内网）。
- `--name` 可以给这台电脑起个名字（默认主机名）。

配好后验证：

```sh
stickbox-agent devices    # 应能看到刚配的屏
stickbox-agent run --once # 手动上报一轮，看是否成功
```

---

## 3. 安装后台服务

```sh
stickbox-agent install
```

- macOS：安装 LaunchAgent（`~/Library/LaunchAgents/ai.stickbox.agent.plist`），
  登录后自动运行；
- Linux：安装 systemd 用户服务（`~/.config/systemd/user/stickbox-agent.service`）；
- 不需要 sudo。卸载用 `stickbox-agent uninstall`。

服务每 10 分钟上报一次。没配对或令牌失效时，它会每小时重试，不会刷屏。

常用命令：

```sh
stickbox-agent status     # 服务器、配置、配对状态、后台服务
stickbox-agent logout     # 注销令牌并删除本机令牌
stickbox-agent unpair <dev>  # 让某块屏解绑（它收到通知后会重新出配对码）
stickbox-agent tz set Asia/Jakarta  # 设置用户时区（见 3b）
```

---

## 3b. 时区（独立于本机）

显示时区存在**服务端**，不需要改本机 `TZ`，也不依赖 agent 所在机器的时区。
优先级：

```
用户设置 > agent 上报 > 服务端 default_tz
```

```sh
stickbox-agent tz set Asia/Jakarta   # IANA 名
stickbox-agent tz set +07:00         # 固定整小时偏移，服务端存成 Etc/GMT-7
stickbox-agent tz show               # 当前生效值 + 来源（用户设置/agent 上报/服务端默认）
stickbox-agent tz clear              # 清除用户设置，回退到 agent/默认
```

- 改完立即重渲染并通知屏换帧，不用等下一次用量上报；
- 非法时区（拼错、`Local`、`+07:30`）会直接报错，不会被静默忽略；
- `+07:00` 这类固定偏移只支持整小时；
- 同一个用户下所有屏共用这一设置。

---

## 3c. 照片（带 SD 槽的屏）

照片存在 SD 卡上，**只对有 SD 槽且卡已初始化的设备生效**；其余屏继续只看板。

```sh
stickbox-agent photo add 照片1.jpg 照片2.png   # 上传，默认自动上屏
stickbox-agent photo add --no-show 照片.jpg    # 只上传，不进播放清单
stickbox-agent photo add --secs 12 照片.jpg    # 上屏并停留 12 秒（5–600）
stickbox-agent photo ls                        # 照片列表 + 设备卡状态
stickbox-agent photo rm abcdef01               # 按 sha 前缀删除（至少 8 位）
stickbox-agent photo init                      # 授权设备初始化 SD 卡（会清空卡）
stickbox-agent photo init --force              # 连 CRC 损坏的卡也允许格式化
```

**上传**

- 服务端负责：方向校正 → 缩放/留白到 1920×1080 → 剥掉 EXIF/GPS/XMP/ICC →
  baseline JPEG（≤250KB）→ 内容寻址。原图不会留在服务器上。
- 单张上限 20MB；每人 200 张 / 64MiB，超了拒绝（不会静默删旧照片）。
- macOS 上遇到 HEIC 会先用 `sips` 转 JPEG；遇到非 sRGB 的照片会尝试用
  `sips -m` 转 sRGB，转不了就照常上传并提示一句（广色域可能略偏淡）。
- agent 不会把原图缓存到别处；只有转换时才在系统临时目录生成副本，上传后删除。
- 同一张照片（最终 JPEG 相同）不会重复占配额；重复上传只返回已有 sha。

**查看**

`photo ls` 显示 sha 前 8 位、大小、停留时长、是否在清单，并附带每台设备最近
一次上报的卡状态（ready/state/plv/已缓存/剩余空间/跳过列表）。老固件没有
`status.photo`，会显示“未上报”。

**初始化（危险操作）**

- SD 卡是**专用整卡**，初始化会清空卡内所有数据，且不能插卡直接拷照片。
- `photo init` 会先要求输入 `yes` 确认；`--force` 还要再确认一次。
- 服务端生成一次性 token（10 分钟有效），agent 只显示前 8 位和到期时间；
  设备只有在清单里拿到有效 token 时才会格式化，格式化后 token 立即失效。
- 没有合法超级块（空白/别的文件系统）才允许普通初始化；超级块存在但 CRC
  都坏的卡算故障，只有 `--force` 授权后才格式化。
- 卡级读失败永远不会自动格式化；受影响设备只上报 `state=fault`，看板照常。

---

## 4. 诊断：`stickbox-agent doctor`

出问题时先跑它，每一项都会给中文修复建议：

```sh
stickbox-agent doctor
```

检查内容：

| 检查 | 说明 |
|---|---|
| claude 可执行文件 | 是否在 PATH（或配置里的绝对路径）、版本多少 |
| 用量读取 | 在白名单环境 + 专用工作目录里跑一次 `/usage`，能否拿到 `rate_limits` |
| 配对令牌 | 有没有令牌；调 `GET /api/agent/devices`，401 就是失效 |
| 服务端连通性 | DNS、TLS 证书、`/healthz` |
| 固件下载路由 | 不存在的固件应返回 stickboxd 的 404，而不是 nginx 的 HTML 404 |
| 后台服务 | 是否安装、日志里最近一次成功上报是什么时候 |
| 时区 | 当前上报的 IANA 时区 |

输出示例：

```
[OK  ] claude 可执行文件
       /usr/local/bin/claude（2.1.296 (Claude Code)）
[OK  ] 用量读取
       五小时 37%（2026-10-17 20:34 重置） / 七天 19%（2026-10-23 08:10 重置）
[OK  ] 配对令牌
       有效（名下 1 台屏）
[OK  ] 服务端连通性
       mqtt.stickbox.ai：DNS / TLS / healthz 正常
[OK  ] 固件下载路由
       不存在的固件返回 stickboxd 自己的 404（/fw/ 已正确反代）
[OK  ] 后台服务
       已安装（~/Library/LaunchAgents/ai.stickbox.agent.plist）；最近一次上报 3 分钟 前
[OK  ] 时区
       Asia/Bangkok
```

有 `FAIL` 时命令退出码为 1，`WARN` 不影响退出码。

日志位置：

- macOS：`~/Library/Logs/stickbox-agent.log`
- Linux：`~/.local/state/stickbox-agent.log`

---

## 5. 常见问题

### 用量读取显示 `rate_limits 为 null` / 429

Claude 的用量接口刚被限流过，本地冷却约 1 小时，期间拿不到限额。**这不是配置
错误**，等一会儿会自动恢复。`doctor` 会明确标成「冷却」。

### 用量读取一直拿不到限额

按顺序排查：

1. **没登录**：终端里运行 `claude` 完成登录，再跑 `doctor`；
2. **第三方端点**：如果设置过 `ANTHROPIC_BASE_URL`、`ANTHROPIC_AUTH_TOKEN`，
   或 `~/.claude/settings.json` 里配置了第三方中转，`/usage` 通常不返回官方
   用量。取消这些设置、改回官方 Claude 订阅后再试；
3. 输出格式变化：手动运行

   ```sh
   claude -p /usage --setting-sources project --output-format stream-json --verbose
   ```

   看有没有 `usage_report` 事件，把输出末尾发给开发者。

### 配对令牌失效（401）

重新运行 `stickbox-agent pair <配对码>`。如果屏已经解绑，先让屏重新出码。

### 服务端连不上

`doctor` 会区分 DNS、TLS 证书、连接被拒和超时：

- 自托管 + 自签证书：`pair` 时加 `--insecure`；
- 公网服务：检查证书链、域名和防火墙；
- 容器部署：确认 `stickboxd` 在运行，`/healthz` 返回 200。

### 后台服务已安装，但画面不更新

看 `doctor` 的「最近一次上报」：

- 显示几小时前：检查 `claude` 是否还能登录、网络是否正常；
- 显示「读不到上报记录」：服务可能没启动。macOS 用
  `launchctl kickstart -k gui/$(id -u)/ai.stickbox.agent`，Linux 用
  `systemctl --user restart stickbox-agent`；
- 日志里有 `not authorized`：令牌失效，重新 `pair`。

### 时区显示不对

先看当前生效值：

```sh
stickbox-agent tz show
```

- 来源是「用户设置」：用 `stickbox-agent tz set <正确时区>` 改；
- 来源是「agent 上报」：agent 会读本机时区（`TZ` 或系统设置），也可以直接
  `stickbox-agent tz set ...` 固定住，不再随本机变化；
- 来源是「服务端默认」：同上，设置一次即可。

---

## 6. 卸载

```sh
stickbox-agent uninstall   # 移除后台服务
stickbox-agent logout      # 注销令牌
rm ~/.local/bin/stickbox-agent
```

配置目录（`agent.json`）可以直接删除；里面只有令牌和 claude 路径。

---

## 7. Windows

`install.sh` 目前只支持 macOS / Linux。Windows 用户：

1. 从 GitHub Release 手动下载 `stickbox-agent-windows-amd64.exe`；
2. 重命名为 `stickbox-agent.exe`，放进 PATH 里的目录；
3. `stickbox-agent pair <配对码>`；
4. `stickbox-agent install`（注册计划任务）。

Windows 上 `doctor` 不检查后台日志（计划任务没有约定的日志文件），其余检查可用。
自动安装脚本和更完整的 Windows 支持后续再补。
