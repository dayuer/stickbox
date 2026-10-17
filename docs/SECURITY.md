# 令牌与安全边界

本文解释服务端为什么这样存令牌、`token_pepper` 是什么，以及轮换/迁移的
操作后果。部署步骤见 [`server/DEPLOY.md`](../server/DEPLOY.md)，运维告警
见 [`docs/OPS.md`](OPS.md)。

## 1. 令牌的形状

- 设备/agent 令牌：`{key_id}.{secret}`
  - `key_id`：6 字节随机数的十六进制（12 字符），明文的定位键；
  - `secret`：24 字节 `crypto/rand` 的 base64url（32 字符），只在下发时
    出现一次。
- 服务端只存 `key_id` 与 `secret` 的哈希，不存明文；日志不打印令牌。

`secret` 是 24 字节均匀随机数，不是人选的密码。离线爆破的搜索空间是
2^192，字典/彩虹表不适用。

## 2. 为什么用 HMAC-SHA256 + pepper

存储格式：

```
$hmac-sha256$v=1$base64(HMAC-SHA256(pepper, "stickbox-token-v1|key_id|secret"))
```

`key_id` 也参与 MAC（域分离）：把一条记录的哈希挪到另一个 `key_id` 下
不会通过，防止哈希在记录之间复用。

- **快**：一次 HMAC 是微秒级。服务端重启后所有设备同时重连时，不会像
  Argon2 那样每个连接占 64MB、把 CPU 全花在清零内存上（T21 实测
  2000 台时 `runtime.memclrNoHeapPointers` 占 65%）。
- **pepper 的价值**：数据库或备份单独泄露时，攻击者没有 `token_pepper`
  就无法离线验证哈希。pepper 只存在 `config.json`，与数据库分开保管。
- **为什么不需要 salt**：secret 本身是高熵随机值；salt 防的是彩虹表，
  而 HMAC 的 pepper 已经让预计算不可行。
- 比较用 `crypto/subtle.ConstantTimeCompare`，避免时序侧信道。

## 3. 旧 Argon2 记录的就地迁移

历史记录是 `$argon2id$...`。服务端仍然能校验它们，但只在**校验成功**
之后立刻改写成 HMAC，然后继续本次登录：

1. 设备重连 → 校验旧哈希（受 8 路信号量限制）→ 通过；
2. `UPDATE devices SET key_hash=... WHERE key_id=...`；
3. 写校验缓存，后续连接走 HMAC。

迁移不需要停机、不需要用户重新配网。没有成功校验过的旧记录会一直保留，
直到那台设备下次上线。迁移失败只记日志，不影响当次登录。

## 4. token_pepper 的操作规则

- 生成：`openssl rand -base64 32`（至少 16 字符）。
- 保存：`/opt/stickbox/config.json`，权限 `600`，属主是运行容器的人；
  不要提交到仓库，不要写进 compose 环境变量，不要打印。
- 缺失或过短：服务端**拒绝启动**（`config: token_pepper 必填...`）。
  这是故意的：静默退回无 pepper 的哈希会让“数据库泄露”重新变成可离线
  爆破。
- 轮换：更换 pepper 等于让所有旧哈希失效。所有设备/agent 必须重新
  配对或重新签发令牌；没有在线迁移路径。当前规模下不做自动轮换。
- 备份：配置备份与数据库备份分开存放；没有 pepper 的数据库备份无法
  恢复登录态。

## 5. 传输与权限

- 对外只有 nginx `443`（HTTPS）和 MQTT TLS `8883`。
- `9443` 只绑 `127.0.0.1`；`9090` 指标端口只绑回环，不经过 nginx。
- MQTT ACL 按密钥自己的 `key_id` 查库判别；待配对密钥只能订阅自己的
  `pair` 主题，不能读 usage/frame。
- 容器只读根文件系统、`cap_drop: ALL`、`no-new-privileges`。

## 6. 连接与资源上限（拒绝服务面）

- MQTT 全局准入：`mqtt_admit_per_sec`，超过直接拒绝 CONNECT，客户端
  按退避重试；被拒次数计入
  `stickbox_rate_limited_total{limiter="mqtt_admit"}`。
- 每连接缓冲：`mqtt_max_writes_pending` / `mqtt_max_inflight` /
  `mqtt_receive_maximum`，默认 64/32/32（mochi 默认 8192/8192/1024，
  每连接约 1.4MB）。
- 帧缓存：磁盘 `frame_cache_max_bytes`、常驻内存
  `frame_memory_max_bytes`，都按最近取用淘汰；启动补帧限速
  `backfill_users_per_sec`，避免重启后渲染风暴。
- HTTP 注册/配对/上报仍走原有的内存限流（见 `internal/httpapi`）。
