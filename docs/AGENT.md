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
```

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

设置 `TZ=Asia/Shanghai`（或你的时区）后重启服务；没有 IANA 时区时服务端会用
默认时区，日期和跨天刷新可能不准。

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
