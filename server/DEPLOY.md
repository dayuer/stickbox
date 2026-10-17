# stickboxd 部署

StickBox 服务端的部署说明。项目方向与接口约定见
[`docs/PLAN.md`](../docs/PLAN.md)。

**部署物只有两样：一个 Go 二进制和一个 sqlite 文件。**
不再需要 CUPS、PPD、字体、Python，也不再保存任何打印作业文件。

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
文件名沿用旧版：已有部署的账号与设备都在这只库里，改名等于让升级丢数据。

生成两把密钥：

```bash
openssl rand -hex 32
```

照 `server/config.example.json` 写 `/opt/stickbox/config.json`，权限 `600`。

## 3. 两把密钥的运维含义（这一节最重要）

| 密钥 | 换掉的后果 |
|---|---|
| `phone_pepper` | **全体用户无法登录**（手机号 HMAC 全部对不上）。**绝不轮换** |
| `phone_key` | 旧的加密号码解不开，但**登录不受影响**。真要轮换得写一次全表重加密 |

两者都在 `config.json` 里，`chmod 600`，不入库。**备份 `config.json` 时它们
一起走，丢了等于用户全体失联。**

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

**`jobs.db` 含个人信息**（加密的手机号与用量）。备份文件必须加密且限权，
不能随手 `scp` 到本地。文件名虽旧，里面已经没有作业数据——打印表在
启动时会一次性清掉。

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

旧部署里的作业服务与独立 mosquitto 都要停掉；broker 已内嵌，
设备侧要同步换密钥，不设并行期。

```bash
sudo systemctl stop stickbox-job.service mosquitto.service
sudo systemctl disable mosquitto.service
sudo cp server/stickboxd.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now stickboxd.service
```

打印功能在本次切换中整条退役：`/api/print`、作业取件、打印机档案与适配
测试端点都已删除，旧固件不能再打印。

**MQTT 主题前缀仍然叫 `printer/{dev}/…`。** 打印退役不改变主题名：固件、
ACL、部署校验脚本都按这个约定写好了，改名会让设备与运维脚本必须同时升
级，没有收益。现在只使用 `printer/{dev}/status`（心跳）与
`printer/{dev}/usage`（用量）两个叶子主题。

## 7. 回滚

回滚到打印版不再受支持——打印代码和数据库表都已删除。如果只是升级
stickboxd 本身出了问题，换回上一版二进制并恢复 `jobs.db` 备份即可。

## 8. 上线验证清单

切换后逐条执行。`$DEV` / `$DEVKEY` / `$APPTOK` 按接口文档拿。

```bash
ss -lntp | grep -E ':(8883|9443)'
```

```bash
openssl s_client -connect mqtt.silkline.id:9443 -servername mqtt.silkline.id </dev/null 2>/dev/null | openssl x509 -noout -dates
```

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://mqtt.silkline.id:9443/api/status
```

### 必须为「失败」的检查

这几条没有用户可见的症状，坏了也不会有人报障，所以必须主动验。

```bash
mosquitto_pub -h mqtt.silkline.id -p 8883 --capath /etc/ssl/certs -u $DEV -P 'bogus.key' -t "printer/$DEV/status" -m '{}'; echo "退出码=$? （非 0 才对）"
```

```bash
mosquitto_pub -h mqtt.silkline.id -p 8883 --capath /etc/ssl/certs -u $DEV -P "$DEVKEY" -t 'printer/aaaaaaaaaaaa/status' -m '{}'; echo "退出码=$? （非 0 才对）"
```

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://mqtt.silkline.id:9443/api/print -H "Authorization: Bearer $APPTOK"
```

依次应当是：连接被拒（非 0）、订阅被拒（非 0）、`404`（打印端点已删除）。

用量链路另验一条：App 读取自己的最新用量应返回 `200`（还没上报过则是
`404`），agent 用手机号 + 验证码登录后能 `POST /api/usage`。

## 9. 运维子命令

```bash
sudo -u stickbox /opt/stickbox/bin/stickboxd -conf /opt/stickbox/config.json device list
```

| 命令 | 用途 |
|---|---|
| `device add <dev> [name]` | 手工签发 device 密钥（调试用；正常走 App 的 enroll） |
| `device list` | 列出全部密钥 |
| `device revoke <key_id>` | 吊销一把 |
| `device unbind <dev>` | **强制解绑**，处理「二手转让但原持有人不配合」 |
| `user list` | 列出用户，只显示手机尾号 |
| `user phone <user_id>` | 解出完整号码，**每次调用写 `/opt/stickbox/audit.log`** |

`device unbind` 绕过所有权检查，所以只能在服务器上执行，不暴露为 API。
`user phone` 是唯一能解出完整号码的入口。
