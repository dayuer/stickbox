# stickboxd 部署

StickBox 服务端的部署说明。项目方向与接口约定见
[`docs/PLAN.md`](../docs/PLAN.md)。

**部署物只有两样：一个 Go 二进制和一个 sqlite 文件。**
没有手机号、短信服务商和 App；设备靠配对码绑定到 agent。

---

## 1. 构建

```bash
cd server/go && CGO_ENABLED=0 go build -ldflags "-X github.com/dayuer/stickbox/server/go/internal/version.Version=$(git describe --tags --always)" -o stickboxd ./cmd/stickboxd
```

`CGO_ENABLED=0` 可行是因为 sqlite 用的是纯 Go 的 `modernc.org/sqlite`。
产物是静态二进制，扔到服务器上就能跑。

## 2. 首次部署

```bash
sudo useradd -r -s /usr/sbin/nologin stickbox
sudo mkdir -p /opt/stickbox/bin
sudo chown -R stickbox:stickbox /opt/stickbox
```

数据库文件 `jobs.db` 会在首次启动时自动创建在 `root` 目录下。
文件名沿用旧版：已有部署的数据都在这只库里，改名等于让升级丢数据。

照 `server/config.example.json` 写 `/opt/stickbox/config.json`，权限 `600`。
配置里只有服务地址与证书目录。

## 3. 配对流程（没有 App、没有手机号）

1. 设备配好 Wi-Fi 后调 `POST /api/device/register {dev}`，拿到设备密钥和
   一个 6 位配对码，显示在电视和 LCD 上。
2. 用户在电脑上运行 `stickbox-agent pair 123456`，调
   `POST /api/agent/pair`。设备归属该 agent 的用户，配对码作废。
3. 已绑定的 MAC 被擦 NVS 重来时，新密钥处于待配对；只有原主人的 agent
   输码才能转正（同时吊销旧密钥），其他人输码返回 409。
4. 解绑用 `POST /api/agent/unpair {dev}`（agent 鉴权）。

配对码 10 分钟有效、一次性；注册和配对失败都有进程内限流。

## 4. 证书

certbot 照旧。**deploy hook 不需要了**——进程按文件 mtime 自己热重载，
不再需要靠重启换证书。

### 私钥读不到时先看补充组

`usermod -aG ssl-cert stickbox` 只改了用户的组，**systemd 服务默认不带补充组**。
单元文件里必须有 `SupplementaryGroups=ssl-cert`，否则服务起来读不到 privkey。

验证热重载生效：

```bash
sudo touch /etc/letsencrypt/live/your.host/fullchain.pem && sudo journalctl -u stickboxd -n 20 | grep 热重载
```

## 5. 备份

**`jobs.db` 含设备密钥、配对关系与用量。** 备份文件必须加密且限权，
不能随手 `scp` 到本地。文件名虽旧，里面没有作业数据——打印表在启动时
会一次性清掉。

## 5b. ⚠ 别用 root 试跑二进制

`sqlite: attempt to write a readonly database (8)` 十有八九是这么来的：
先用 root 手动跑了一次 `stickboxd`，它把 `jobs.db` 建成了 root 属主，
之后服务以 `stickbox` 身份启动就写不进去。

排查一眼就够：

```bash
ls -la /opt/stickbox/*.db
```

修法：

```bash
chown -R stickbox:stickbox /opt/stickbox && systemctl restart stickboxd
```

要试跑就带上身份：`sudo -u stickbox /opt/stickbox/bin/stickboxd -conf ...`

## 6. 从旧服务切换

旧部署里的作业服务与独立 mosquitto 都要停掉；broker 已内嵌。

```bash
sudo systemctl stop stickbox-job.service mosquitto.service
sudo systemctl disable mosquitto.service
sudo cp server/stickboxd.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now stickboxd.service
```

打印功能在本次切换中整条退役。**MQTT 主题前缀仍然叫 `printer/{dev}/…`**：
固件、ACL、部署校验脚本都按这个约定写好了，改名没有收益。现在只使用
`printer/{dev}/status`（心跳）、`printer/{dev}/usage`（用量）与
`printer/{dev}/pair`（配对通知）三个叶子主题。

## 7. 回滚

回滚到打印版不再受支持——打印代码和数据库表都已删除。如果只是升级
stickboxd 本身出了问题，换回上一版二进制并恢复 `jobs.db` 备份即可。

## 8. 上线验证清单

```bash
ss -lntp | grep -E ':(8883|9443)'
```

```bash
openssl s_client -connect your.host:9443 -servername your.host </dev/null 2>/dev/null | openssl x509 -noout -dates
```

注册一台测试设备并配对：

```bash
curl -s -X POST https://your.host:9443/api/device/register \
  -H 'Content-Type: application/json' -d '{"dev":"f412fa87c9e0"}'
# → {"token":"...","code":"123456","expires_at":...}
curl -s -X POST https://your.host:9443/api/agent/pair \
  -H 'Content-Type: application/json' -d '{"code":"123456","name":"test"}'
# → {"dev":"f412fa87c9e0","user_id":"...","token":"..."}
curl -s https://your.host:9443/api/agent/devices \
  -H "Authorization: Bearer $AGENTTOK"
```

### 必须为「失败」的检查

这几条没有用户可见的症状，坏了也不会有人报障，所以必须主动验。

```bash
mosquitto_pub -h your.host -p 8883 --capath /etc/ssl/certs -u $DEV -P 'bogus.key' -t "printer/$DEV/status" -m '{}'; echo "退出码=$? （非 0 才对）"
```

```bash
mosquitto_pub -h your.host -p 8883 --capath /etc/ssl/certs -u $DEV -P "$DEVKEY" -t 'printer/aaaaaaaaaaaa/status' -m '{}'; echo "退出码=$? （非 0 才对）"
```

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://your.host:9443/api/device/register \
  -H 'Content-Type: application/json' -d '{"dev":"zz"}'
# → 400
```

依次应当是：连接被拒（非 0）、订阅被拒（非 0）、`400`。

## 9. 运维子命令

```bash
sudo -u stickbox /opt/stickbox/bin/stickboxd -conf /opt/stickbox/config.json device list
```

| 命令 | 用途 |
|---|---|
| `device add <dev> [name]` | 手工签发 device 密钥（调试用；正常走设备注册） |
| `device list` | 列出全部密钥（含「待配对」状态） |
| `device revoke <key_id>` | 吊销一把 |
| `device unbind <dev>` | **强制解绑**，处理二手转让 |
| `user list` | 列出用户（没有手机号） |

`device unbind` 绕过所有权检查，所以只能在服务器上执行，不暴露为 API。
