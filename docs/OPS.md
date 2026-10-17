# 运维手册

正式服务端 `stickboxd` 的部署、升级、回滚、证书与告警处理。部署细节
（compose、nginx 原文、备份安装）见 [`server/DEPLOY.md`](../server/DEPLOY.md)。

```
stickbox-agent ──HTTPS 443──▶ nginx ──TLS──▶ 127.0.0.1:9443 stickboxd
                                                       ▲
ESP32 设备 ────────────── MQTTS 8883 ───────────────────┘
```

- 对外：443（API）、8883（MQTT TLS）
- 只对本机：9443（HTTP API 源站）、9090（Prometheus 指标）
- 容器：`network_mode: host`、只读根文件系统、`cap_drop: ALL`
- 数据：宿主机 `/opt/stickbox/data/jobs.db`（容器内 `/data`）

---

## 1. 日常检查

```bash
cd /opt/stickbox
docker compose ps
docker inspect --format '{{.State.Health.Status}}' stickboxd   # healthy
curl -sk https://127.0.0.1:9443/healthz                        # ok
curl -s  https://mqtt.stickbox.ai/healthz                      # 走 nginx 也应为 ok
curl -s  http://127.0.0.1:9090/metrics | head
docker logs --since 30m stickboxd
ss -lntp | grep -E ':(443|8883|9443|9090)\b'
```

健康检查只回答 200/503：sqlite 能读、MQTT broker 在监听。503 时先看
`docker logs`，再按第 6 节排查。

> `-conf` 是全局参数，必须放在子命令**之前**：
> `stickboxd -conf /etc/stickbox/config.json healthcheck`。
> compose 的 healthcheck 已按这个顺序写。

---

## 2. 部署与升级

### 首次部署

见 [`server/DEPLOY.md`](../server/DEPLOY.md) 第 4b 节。要点：

- `config.json` 的 `root` 必须是 `/data`（宿主机 `/opt/stickbox/data`）；
- `http_addr=127.0.0.1:9443`、`mqtt_addr=:8883`、
  `metrics_addr=127.0.0.1:9090`；
- compose 用 `network_mode: host`，这样 nginx 反代过来的来源是回环，
  服务端才采用 `X-Forwarded-For`。

### 升级

```bash
cd /opt/stickbox
# 1. 记住上一版
docker tag stickboxd:${STICKBOX_TAG:-latest} stickboxd:prev

# 2. 拉/构建新镜像（发布流水线产出的 tag）
docker pull ghcr.io/<owner>/stickboxd:v0.2.0

# 3. 切标签并重启
echo 'STICKBOX_TAG=v0.2.0' | sudo tee .env
sudo docker compose up -d
docker inspect --format '{{.State.Health.Status}}' stickboxd
```

升级前确认当天备份成功（第 4 节）。数据库迁移在启动时自动执行；
如果要从新版本回滚到旧版本，先恢复升级前的备份。

### 回滚

```bash
cd /opt/stickbox
echo 'STICKBOX_TAG=prev' | sudo tee .env
sudo docker compose up -d
docker inspect --format '{{.State.Health.Status}}' stickboxd
```

`stickboxd:prev` 是升级前打的标签；回滚不需要重新构建。回滚后检查
`/healthz` 和设备是否重新上线（`stickbox_online_devices`）。

---

## 3. 证书

- certbot 用 **webroot** 续期；80 端口由隐式默认 server 提供
  acme-challenge，站点文件里**不要**写 `listen 80`。
- 证书路径：`/etc/letsencrypt/live/mqtt.stickbox.ai/{fullchain,privkey}.pem`，
  只读挂进容器。
- `stickboxd` 每次 TLS 握手检查文件 mtime，变化就热重载，**不需要**
  deploy hook，也不需要重启容器。

验证：

```bash
sudo touch /etc/letsencrypt/live/mqtt.stickbox.ai/fullchain.pem
docker logs --since 2m stickboxd | grep 热重载
openssl s_client -connect mqtt.stickbox.ai:443 -servername mqtt.stickbox.ai </dev/null 2>/dev/null \
  | openssl x509 -noout -dates
```

nginx 站点文件：`/etc/nginx/sites-available/mqtt.stickbox.ai`，
启用链接在 `/etc/nginx/sites-enabled/`。改完：

```bash
sudo nginx -t && sudo systemctl reload nginx
```

---

## 4. 备份

- 每天 03:17 由 cron 跑 `/opt/stickbox/backup.sh`；
- 容器内 `stickboxd backup` 用 `VACUUM INTO` 生成一致性快照；
- `age` 公钥加密到 `/opt/stickbox/backups/jobs-<UTC>.db.age`；
- 保留最近 14 份；私钥**不在服务器上**；
- 明文快照只在 `/data/backup` 存在几秒，脚本退出时删除。

检查：

```bash
ls -lh /opt/stickbox/backups | tail
tail -20 /var/log/stickbox-backup.log
```

恢复步骤见 `server/DEPLOY.md` 第 5 节。恢复前先停容器，替换
`data/jobs.db` 并删掉 `-wal`/`-shm`，再启动。

---

## 5. 指标与告警

`GET http://127.0.0.1:9090/metrics`（Prometheus 文本）。

| 指标 | 含义 | 建议告警 |
|---|---|---|
| `stickbox_online_devices` | 最近 120s 发过心跳的设备数 | 长时间为 0（按实际装机量） |
| `stickbox_mqtt_connections` | 当前 MQTT 连接数 | 持续为 0 且设备在线 → broker/证书问题 |
| `stickbox_usage_reports_total` | 成功用量上报累计 | 白天 `rate(...[10m]) == 0` |
| `stickbox_render_seconds` | 渲染耗时直方图 | p95 > 5s |
| `stickbox_render_failures_total` | 渲染失败累计 | `increase(...[10m]) > 0` |
| `stickbox_frame_package_bytes` | 帧包大小直方图 | 接近 256KB（flash 槽上限） |
| `stickbox_pair_total{result="ok"/"fail"}` | 配对成功/失败 | 失败比例突增（扫号/限流） |
| `stickbox_rate_limited_total{limiter=...}` | 限流命中；`mqtt_admit` 是新连接准入 | 突增说明被刷或重连风暴 |
| `stickbox_pending_takeover_rejected_total` | pending 密钥试图顶掉在线设备被拒 | 突增可能是顶号攻击/旧设备重连 |
| `stickbox_frame_pending_users` | 帧渲染排队或防抖中的用户数 | 持续堆积说明渲染慢/锁竞争 |

示例 PromQL：

```promql
rate(stickbox_usage_reports_total[10m])
histogram_quantile(0.95, sum(rate(stickbox_render_seconds_bucket[5m])) by (le))
increase(stickbox_render_failures_total[10m])
sum(rate(stickbox_rate_limited_total[5m])) by (limiter)
```

9090 **不要**加进 nginx，也不要开防火墙。抓取端可以在本机跑
Prometheus，或通过 SSH 隧道访问。`/healthz` 可以经 nginx 443 暴露给
uptime 监控（只返回 200/503，无内部信息）。

---

## 6. 常见故障

### `/healthz` 返回 503

1. `docker logs --since 10m stickboxd`；
2. sqlite：`ls -ld /opt/stickbox/data`、磁盘 `df -h`，确认容器内
   `root=/data` 且可写；
3. broker：`ss -lntp | grep 8883`，证书是否过期/读不到。

### 设备全部离线

- `stickbox_mqtt_connections` 是否为 0；
- 8883 证书链是否完整（`openssl s_client -connect ...:8883 -servername ...`）；
- 设备侧 Wi-Fi/配对码，见 `docs/API.md`；
- 最近是否改过 ACL/用户名规则（username 必须等于 dev）。

### 配对失败率升高

- 看 `stickbox_rate_limited_total{limiter=~"pair_.*"}`；
- 配对码 10 分钟过期、一次性；让用户重新出码；
- 服务端时间是否漂移（TLS/日志时间）。

### 证书续期后客户端仍报错

- 确认 `docker logs` 有「证书已热重载」；
- MQTT 和 nginx 用同一份证书；`fullchain.pem` 必须含中间证书；
- 设备系统时间是否正确（SNTP）。

### 磁盘/数据库异常

- `jobs.db` 在 `/opt/stickbox/data`；不要用 root 手工跑二进制把它变成
  root 属主（见 DEPLOY 5b）；
- 备份日志和 `docker system df` 定期检查；容器日志已按 10MB×5 轮转。

---

## 7. 安全边界

- `/metrics` 只监听回环；不要改 `metrics_addr` 为 `0.0.0.0`。
- 9443 只绑 `127.0.0.1`；对外只有 nginx 443。
- 备份只有 age 密文；私钥离线保存，丢了就无法恢复历史备份。
- 日志不打印令牌/用量明细；`internal/broker` 有回归测试防止泄露。
- 容器只读根文件系统、`cap_drop: ALL`、`no-new-privileges`；不要为了
  调试加 `privileged`。
