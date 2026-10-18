# 上板验收清单

每个固件版本发布前，按这份清单在真实板子 + 真实电视上跑一遍。**任何一项
FAIL 都阻断发布**；WARN 需要记录原因并由 USB-TV 确认。

- 适用：`rom/` 下的固件（开发构建或发布构建，见第 7 项）
- 硬件：ESP32-S3 板 + 三星 UA60DU7000（Room C）或 `docs/SUPPORTED-TVS.md`
  中已验证的电视
- 服务端：优先用 **staging**；第 3、6、7 项需要停服/改 manifest，**不要对
  生产环境做**
- 证据：全程串口日志 + 关键画面照片，日志存成
  `/tmp/board-accept-<版本>-<日期>.log`，随发布记录归档

先设好变量（端口和电视按现场改）：

```bash
export PORT=/dev/cu.usbmodem1301
export TV=192.168.3.18
export HOST=mqtt.stickbox.ai          # 被测服务端；staging 时改成对应域名
```

### 烧录被测固件

```bash
source ~/esp/esp-idf-v5.5/export.sh
cd rom
idf.py -p "$PORT" --after watchdog_reset flash
```

烧完用串口确认版本：

```
运行槽位 <ota_0/ota_1>，版本 <被测版本>，状态 已确认
```

**`--after watchdog_reset` 不能省**，否则这块板子的 USB-JTAG 可能烧完不启动。

> 下文的 `idf.py` / `esptool.py` 都要求先 `source ~/esp/esp-idf-v5.5/export.sh`。

---

## 0. 共享资源约定（开始前 / 结束后必做）

板子和电视都只有一套，同一时间只能有一个推流方。开始前先确认没有别的
会话在占用；有冲突先找 USB-TV 协调。

### 开始前

1. **停掉模拟器**（如果它在给电视推流）。在跑 `stickbox-sim` 的终端按
   `Ctrl-C`，或用：

   ```bash
   pkill -INT -f stickbox-sim || true
   pgrep -fl stickbox-sim        # 应无输出
   ```

   模拟器日志最后一行应为 `退出`。

2. **停住板子**（避免它自己推流/抢串口）：

   ```bash
   esptool.py -p "$PORT" --after no_reset chip_id
   ```

   看到 `Staying in bootloader.` 才算停住。注意 `--after` 默认是
   `hard_reset`，不带它会把 app 放起来。

3. **确认电视空闲**：电视不应还停在上一路 `video/avi` 画面。必要时用
   遥控器按 Stop/返回。

### 结束后（恢复现场）

1. 停住板子：

   ```bash
   esptool.py -p "$PORT" --after no_reset chip_id
   ```

2. 恢复板子（如果现场原本是板子在跑）：

   ```bash
   esptool.py -p "$PORT" --after watchdog_reset chip_id
   ```

   或拔插 USB。串口应重新出现 `=== StickBox 电视看板 ===`。

3. 恢复模拟器（如果现场原本是模拟器在跑）：

   ```bash
   cd server/go
   go run ./cmd/stickbox-sim --server "https://$HOST" \
     --state "$HOME/.stickbox-sim-state" --listen :8090 --push-tv "$TV"
   ```

   首次运行会打印配对码；已配对会打印 `设备 <dev> 已配对`。确认电视重新
   出画后再离开。

> 串口是独占的：`serial_log.py`、`idf.py flash`、`esptool.py` 不要同时开。
> 用完先 `Ctrl-C` 释放。

---

## 1. 冷启动：5 秒开始拉流，16 秒真实帧上屏

**前置**：板子已烧录被测固件、Wi-Fi 已配好、电视开着且空闲。

**步骤**

1. 在仓库根目录开始带时间戳的串口日志（工具只读、不复位）：

   ```bash
   python3 -u rom/tools/serial_log.py "$PORT" 115200 \
     | perl -MTime::HiRes=time -ne 'printf "%.3f %s", time, $_' \
     | tee "/tmp/board-accept-$(date +%Y%m%d-%H%M).log"
   ```

2. 触发一次干净启动（或按板子 RST）：

   ```bash
   esptool.py -p "$PORT" --after watchdog_reset chip_id
   ```

3. 计时基准取开机横幅：

   ```
   === StickBox 电视看板 ===
   ```

**预期日志**

```
电视开始拉流（连接数 1）
...（缓存命中时）用 flash 里的帧起播，版本 <ver>
...（或新拉取）新帧已上屏，版本 <ver>，堆 <n>
```

**通过判据**

- `电视开始拉流` 的时间戳 − 开机横幅 ≤ **5 秒**
- 电视上出现真实看板（不是预置页/配对页）；对应
  `新帧已上屏`（新拉取）或 `用 flash 里的帧起播`（缓存命中）之一，
  时间戳 − 开机横幅 ≤ **16 秒**
- 若走缓存命中，画面必须是看板内容（时钟带/用量），不能是纯色页

快速对账：

```bash
awk '/=== StickBox 电视看板 ===/{t0=$1}
     /电视开始拉流/{printf "拉流 %.1f s\n", $1-t0}
     /新帧已上屏/{printf "新帧 %.1f s\n", $1-t0}
     /用 flash 里的帧起播/{printf "缓存帧 %.1f s\n", $1-t0}' \
  /tmp/board-accept-*.log
```

---

## 2. 内存：稳态 ≥110K，最低 ≥40K

**前置**：第 1 项通过，电视正在拉流。

**步骤**

1. 保持推流 **至少 10 分钟**，不重启、不重新配对。统计开机 5 分钟之后的
   采样（前 5 分钟还有开机取帧/TLS 峰值，不算稳态）。
2. 期间不要同时开 OTA 下载或额外的 TLS 客户端。

**预期日志**（每 60 秒一条；心跳每 30 秒一条）

```
推流统计 frames=<n> clients=1 heap=<bytes> min=<bytes>
心跳 heap=<bytes> min=<bytes>
```

**通过判据**

- 开机 5 分钟之后的每条 `推流统计` 中：
  - `heap=` ≥ **110000**
  - `min=` ≥ **40000**
- 全程不出现：
  - `空闲堆 <n> < 60000，本轮不取帧（推流优先）`
  - `空闲堆 <n> < 90000，本轮不升级（推流优先）`
  - 重启、abort、看门狗复位
- `clients=1` 且 `frames` 持续增长

对账：

```bash
grep '推流统计' /tmp/board-accept-*.log
awk '/=== StickBox 电视看板 ===/{t0=$1}
     t0 && ($1-t0)>=300 && /推流统计/{
       for(i=1;i<=NF;i++){
         if($i ~ /^heap=/){split($i,a,"="); if(a[2]<110000){print "FAIL heap",a[2]; bad=1}}
         if($i ~ /^min=/){split($i,a,"="); if(a[2]<40000){print "FAIL min",a[2]; bad=1}}
       }
     } END{exit bad}' /tmp/board-accept-*.log; echo "内存检查退出码=$?"
```

> 说明：`min` 是开机以来的最低堆（`esp_get_minimum_free_heap_size`），
> 40K 是底线；`heap` 是当前空闲堆，稳态要保持在 110K 以上。数值单位是字节。

---

## 3. 服务端停机 90 秒再恢复：密钥保留、退避重连、推流不断

**前置**：板子连的是 **staging/本地测试服务端**（不要停生产），电视正在
拉流，板子已配对。

**步骤**

1. 记下停服前最后一条 `推流统计 frames=<n>`。
2. 在服务端停 90 秒：

   ```bash
   # Docker 部署
   cd /opt/stickbox
   sudo docker compose stop stickboxd
   sleep 90
   sudo docker compose start stickboxd

   # 非 Docker
   # sudo systemctl stop stickboxd; sleep 90; sudo systemctl start stickboxd
   ```

3. 恢复后确认健康：

   ```bash
   sudo docker exec stickboxd /stickboxd -conf /etc/stickbox/config.json healthcheck
   echo "healthcheck 退出码=$?（应为 0）"
   ```

**预期日志**

```
与云端断开，<4000±20%> ms 后重连
与云端断开，<8000±20%> ms 后重连
...
已连接云端 mqtts://...
推流统计 frames=<更大的 n> clients=1 ...
```

**通过判据**

- 停机期间出现至少 3 条 `与云端断开，... ms 后重连`，间隔递增（约
  4s → 8s → 16s → 32s → 60s 封顶，抖动 ±20%），不是固定 2s 死循环
- **密钥保留**：全程没有 `已清除设备密钥（保留服务器地址）`，也没有重新
  注册、新配对码
- 恢复后 ≤75 秒出现 `已连接云端`，心跳恢复（退避最长 60 秒 + 20% 抖动）
- **推流不断**：没有 `流断开`；停机期间和恢复后 `clients=1`，`frames`
  持续增长，电视画面没有中断（时钟带每分钟继续更新）
- 停机期间允许出现 `连不上 .../device/frame` / `取帧失败`（取帧会按
  15/30/60 秒退避），这是预期，不算失败

---

## 4. 服务端 `device unbind`：3 次 0x05 → 清理 → 重启进配对页

**前置**：板子在线、已配对、电视在看板；知道设备的 dev（串口日志里的
`设备 ID: <dev>`，或服务端 `device list`）。

**步骤**

1. 在服务端强制解绑（**只对测试设备执行**）：

   ```bash
   sudo docker exec stickboxd /stickboxd -conf /etc/stickbox/config.json \
     device unbind <dev>
   ```

2. 观察串口最多 90 秒。解绑是另一个进程直接改库，要等设备下一次心跳
   （≤30 秒）才踢线，不要提前判定失败。

**预期日志（按顺序）**

```
云端明确拒绝认证 0x05（1/3）
云端明确拒绝认证 0x05（2/3）
云端明确拒绝认证 0x05（3/3）
连续被拒，判定密钥失效——清密钥重新注册
cleanup: 作废帧槽
cleanup: 作废 frames 槽        # frames 分区正常时会出现
cleanup: 停止 MQTT client
cleanup: 重启

...（重启后）
无设备密钥：作废 flash 帧，不从旧帧起播
frames 槽已作废，切回预置页
配对码 <6 位> 已上电视，条带 <n> B
```

**通过判据**

- 连续收到 **3 次** `0x05`（1/3、2/3、3/3），中间没有 0x03
- 出现三条 `cleanup:`：`作废帧槽`、`停止 MQTT client`、`重启`；
  另有一条 `cleanup: 作废 frames 槽` 属正常，不要求恰好三条
- 重启后不再播放旧看板：电视回到预置页/配对页，日志有
  `无设备密钥：作废 flash 帧，不从旧帧起播`
- 重新注册成功，电视和 LCD 出现新的 6 位配对码

---

## 5. 重新配对：配对码出现，30 秒内新帧上屏

**前置**：第 4 项刚完成，电视显示配对码。

**步骤**

1. 记下电视/LCD 上的 6 位配对码。
2. 在 Mac 上配对（本机联调自签证书时加 `--insecure`）：

   ```bash
   stickbox-agent pair --server "https://$HOST" <配对码>
   ```

3. 立刻触发一次用量上报，让服务端渲染新帧：

   ```bash
   stickbox-agent run --once
   ```

**预期日志**

```
配对码 <6 位> 已上电视，条带 <n> B      # 配对前就应出现
服务端通知新帧版本 <ver>
开始取帧：<bytes> 字节，版本 <ver>，槽 <n>，堆 <n>
配对成功，切到看板
新帧已上屏，版本 <ver>，堆 <n>
```

**通过判据**

- 配对前电视/LCD 能看到 6 位配对码；码过期会自动换新
  （`配对码过期，换新码`）
- `stickbox-agent pair` 成功后，≤ **30 秒**出现
  `配对成功，切到看板` **且** `新帧已上屏`
- 电视从配对页切到真实看板；画面版本与 `stickboxd device show <dev>`
  里的帧版本一致
- 不出现 `stale_key` 循环，不反复重新注册

---

## 6. 注册配额：1 小时最多 3 次，超限停在配对页

**前置**：使用 staging；第 4 项的流程可重复执行。计数写在 NVS，软重启
不会清零。设备必须先完成 SNTP（时间是当前年份）；开机早期时间没对上时
配额检查会放行，看不到限流日志。

**步骤**

1. 重复第 4 项的 `device unbind` 流程 3 次。每次设备都会清密钥、重启、
   自动注册一次；成功注册后日志会有：

   ```
   注册计数 1/3（本小时）
   注册计数 2/3（本小时）
   注册计数 3/3（本小时）
   ```

2. 第 4 次执行 `device unbind`，观察设备注册环节。

**预期日志**

```
1 小时内已注册 3 次，拒绝继续注册
注册频率超限，30 秒后重试
```

LCD 显示 `注册过于频繁，稍后重试`；每 30 秒重试一次。

**通过判据**

- 第 4 次不再发出注册请求（服务端没有新的 pending 密钥），也没有新的
  `配对码 ... 已上电视`
- 电视停在配对页/预置页，不播放任何旧看板
- 不出现 `cleanup:` 循环或反复重启（超限路径应只退避，不擦 flash）
- 恢复现场：等满 1 小时窗口，或 `idf.py -p "$PORT" erase-flash flash`
  后重新配网（会清空 Wi-Fi 与计数）

---

## 7. OTA 回滚：坏版本崩溃后回滚、记录坏版本、不再重复下载

**前置**

- staging 服务端，能改 `config.json` 和 `<root>/fw/`
- 板子当前的签名状态与坏版本一致：
  - 串口 `OTA 签名校验：关（仅限开发构建）` → 用开发构建做坏版本
  - `OTA 签名校验：开` → 坏版本必须用发布用的同一把 `ota_key.pem` 签名，
    否则下载阶段就失败，测不到回滚
- OTA 下载走 HTTPS，测试服证书必须被 `esp_crt_bundle` 信任；自签证书
  联调构建需定义 `STICKBOX_INSECURE_TLS`

**步骤**

1. 编译一个「启动 10 秒后 abort」的坏版本（版本号必须不同）：

   ```bash
   source ~/esp/esp-idf-v5.5/export.sh
   cd rom
   mkdir -p build-bad
   printf 'CONFIG_STICKBOX_OTA_SUICIDE=y\n' > build-bad/suicide.defaults
   idf.py -B build-bad -DSDKCONFIG=build-bad/sdkconfig \
     -DSDKCONFIG_DEFAULTS="sdkconfig.defaults;build-bad/suicide.defaults" \
     -DPROJECT_VER=v9.9.9-bad build
   ```

   若板子是签名构建，把 `sdkconfig.release` 加进 defaults，并确保
   `rom/ota_key.pem` 存在（**不要入库**）：

   ```bash
   idf.py -B build-bad -DSDKCONFIG=build-bad/sdkconfig \
     -DSDKCONFIG_DEFAULTS="sdkconfig.defaults;sdkconfig.release;build-bad/suicide.defaults" \
     -DPROJECT_VER=v9.9.9-bad build
   ```

2. 生成 manifest 片段并放到服务端：

   ```bash
   python3 tools/make_ota_manifest.py build-bad/stickbox.bin v9.9.9-bad /tmp/bad.json
   cat /tmp/bad.json
   ```

   在服务端把 bin 放到 `<root>/fw/`（compose 部署默认是
   `/opt/stickbox/data/fw`，容器内 `/data/fw`；以 `config.json` 的
   `root` 为准）：

   ```bash
   mkdir -p /opt/stickbox/data/fw
   cp stickbox.bin /opt/stickbox/data/fw/app-v9.9.9-bad.bin
   ```

   URL 走 nginx 的 `/fw/` 反代，conf 见
   `server/deploy/nginx-stickbox.conf`；可以用
   `curl -sI https://<staging-host>/fw/app-v9.9.9-bad.bin` 先确认能拿到
   `200`/`Content-Length`。

   把 `version` / `sha256` / `size` 和 URL 写进 `config.json`：

   ```json
   "firmware": {
     "version": "v9.9.9-bad",
     "url": "https://<staging-host>/fw/app-v9.9.9-bad.bin",
     "sha256": "</tmp/bad.json 里的值>",
     "size": <同上>,
     "rollout_percent": 100
   }
   ```

   重启服务端让配置生效（只有证书是热重载）：

   ```bash
   cd /opt/stickbox && sudo docker compose restart stickboxd
   ```

3. 重启板子。OTA 首次检查在**开机 10 分钟后**，之后每 24 小时一次。
   预留至少 **25 分钟**：第一次检查（10 分钟）+ 回滚重启后再等一次检查
   （10 分钟）才能验证「不再重复下载」。

**预期日志**

```
有新版本 v9.9.9-bad（当前 <ver>）
开始下载 https://<staging-host>/fw/app-v9.9.9-bad.bin（堆 <n>）
新固件已写入备用槽，重启进入试运行

...（重启后）
运行槽位 ota_x，版本 v9.9.9-bad，状态 待验证（健康检查通过前不落定）
本次是 OTA 后试运行：180 秒内没通过健康检查，重启将回滚
OTA 回滚测试固件：现在 abort()

...（bootloader 回滚后）
运行槽位 ota_0，版本 <ver>，状态 已确认

...（下一次检查，开机 10 分钟后）
版本 v9.9.9-bad 之前已判定不可用，跳过
```

若坏版本是「能启动但连不上云端」而不是 abort，则走健康检查超时路径，
日志为：

```
健康检查 180 秒未通过，判定这版固件不可用，回滚上一槽
已记下坏版本 v9.9.9-bad，以后不再下载它
标记本版不可用并回滚
```

**通过判据**

- 坏版本被下载并写入备用槽，重启后确实崩溃/未通过健康检查
- 设备自动回滚到上一版（版本号恢复），服务恢复（联网 + MQTT + 推流）
- 坏版本被记住：出现 `已记下坏版本 ...` 或
  `版本 ... 之前已判定不可用，跳过`
- **不再重复下载**：之后再经过至少一次 OTA 检查，没有第二条
  `开始下载 ...app-v9.9.9-bad.bin`
- 测完把服务端 `firmware` 配置恢复（或 `rollout_percent: 0`）

---

## 8. 固件里的服务器地址：`check_embedded_hosts.py`

**前置**：拿到**将要发布/上板的那一个** `stickbox.bin`（不要拿旧构建）。

**步骤**

```bash
cd rom
# 发布构建；开发构建换成 build/stickbox.bin
python3 tools/check_embedded_hosts.py build-release/stickbox.bin "$HOST"
echo "退出码=$?"
```

**通过判据**

- 退出码 0，输出：

  ```
  check_embedded_hosts: OK（<host>）
  ```

- 不允许出现：

  ```
  ✗ 固件包含非白名单主机: <host>
  ```

  （典型原因：构建时用了本机被 gitignore 的 `cloud_creds.h`，里面还是
  旧服务器 `silkline`）

- 发布走 `tools/release_build.sh` 时这一步已内置；手工构建也必须跑一次
- 注意脚本只匹配 ASCII 的 `mqtts://` / `https://` 主机名，且要求主机名
  带 `.`；换域名后记得把新域名作为参数传进去

---

## 9. 共享资源：板子与电视的占用约定

这一节是第 1～8、10～18 项的前置/收尾，也是多人协作时的硬约定。

| 动作 | 命令 | 确认方式 |
|---|---|---|
| 停模拟器 | 前台 `Ctrl-C`，或 `pkill -INT -f stickbox-sim` | `pgrep -fl stickbox-sim` 无输出；日志 `退出` |
| 停板子 | `esptool.py -p "$PORT" --after no_reset chip_id` | 输出 `Staying in bootloader.` |
| 恢复板子 | `esptool.py -p "$PORT" --after watchdog_reset chip_id` | 串口出现 `=== StickBox 电视看板 ===` |
| 恢复模拟器 | 见第 0 节命令 | 日志 `设备 <dev> 已配对`，电视出画 |
| 释放串口 | 对 `serial_log.py` 按 `Ctrl-C` | 端口可被下一次 `idf.py`/`esptool` 打开 |

规则：

- **同一时间只允许一个推流方**（板子或模拟器），否则电视会被两边抢屏。
- 测试前先停模拟器、停板子；测试结束后按现场原状态恢复，不确定就问
  USB-TV，不要留下占用。
- 烧录必须带 `--after watchdog_reset`，否则烧完可能不启动。
- `idf.py monitor` / `screen` / 裸 pyserial 会拉 DTR/RTS 把板子按进下载
  模式，只能用 `rom/tools/serial_log.py`。
- 第 3、6、7、10、11 项会停服或改 OTA manifest，只在 staging 做；
  第 4 项的 `device unbind` 只对测试设备执行。
- 第 12–18 项要用**测试 SD 卡**（卡内数据会被清空）；先确认卡不是别人的，
  开始前记录卡状态。照片测试不要和烧录、OTA、电视长跑同时进行。

---

## 10. 已签名的新版本经 OTA 升级成功

**前置**

- 板子当前跑的是**签名发布构建**，串口有
  `OTA 签名校验：开`（开发构建是「关」，本项无意义）
- staging 服务端，能改 `config.json` 和 `<root>/fw/`
- 用 `rom/tools/release_build.sh <tag> <正式私钥>` 构建出待发布固件
  （见 `docs/RELEASE.md` 3.2），版本号与当前不同

**步骤**

1. 把 `build-release/app.bin` 放到 `<root>/fw/app-<tag>.bin`，按
   `docs/RELEASE.md` 第 4 节写 manifest，`rollout_percent: 100`。
2. 重启 stickboxd，重启板子；OTA 首次检查在开机 10 分钟后。
3. 全程保持串口日志和电视画面。

**预期日志**

```
有新版本 <tag>（当前 <old>）
开始下载 https://<staging-host>/fw/app-<tag>.bin（堆 <n>）
新固件已写入备用槽，重启进入试运行

...（重启后）
运行槽位 ota_x，版本 <tag>，状态 待验证（健康检查通过前不落定）
健康检查通过（<n> 秒）：已联网 + 云端已连上
健康检查通过，新固件已落定
```

**通过判据**

- 版本切到 `<tag>`，出现 `健康检查通过，新固件已落定`，没有回滚
- MQTT 重新连上，电视恢复推流；`推流统计` 继续增长
- `ota_version()` 对应的心跳 `fw` 字段也是 `<tag>`（服务端可查）

---

## 11. 未签名固件被设备拒绝

**前置**

- 同第 10 项：当前固件必须是签名发布构建（`OTA 签名校验：开`）
- 准备一个**未签名**的开发构建（`idf.py build`，版本号不同于当前固件；
  不要用 `sdkconfig.release`）

**步骤**

1. 把未签名的 `stickbox.bin` 放到 `<root>/fw/`，manifest 指向它，
   `rollout_percent: 100`。
2. 重启 stickboxd，重启板子，等待开机 10 分钟后的 OTA 检查。

**预期日志**

```
有新版本 <unsigned-ver>（当前 <tag>）
开始下载 https://<staging-host>/fw/app-<unsigned-ver>.bin（堆 <n>）
升级失败 ESP_ERR_OTA_VALIDATE_FAILED，继续用当前固件
```

**通过判据**

- 出现 `升级失败` 且错误含 `VALIDATE`/签名校验失败；**没有**
  `新固件已写入备用槽`
- 设备不重启、不回滚，继续运行当前签名版本；电视推流不中断
- 未签名版本不会被记住成「坏版本」（它从未写进备用槽），后续检查可能
  再次尝试下载——测完把 `rollout_percent` 改回 `0` 或恢复 manifest
- 如果日志是 `OTA 签名校验：关`，说明板子跑的是开发构建，本项无效，
  先换成签名版本再测

---

## 12. `photo init` 授权格式化

**适用**：带 SD 槽的板子。无 SD 槽的构建把第 12–18 项记为 N/A。

**步骤**

1. 插一张新卡（或用户确认可清空的卡，可以是 FAT/空白），开机。
2. 未执行 `photo init` 时，串口应**没有**任何格式化动作；`stickbox-agent photo ls`
   显示设备 `state=uninit`。
3. Mac 上执行 `stickbox-agent photo init`，输入 `yes` 确认。10 分钟内设备
   拉到清单后应格式化。
4. 再执行一次 `photo init --force`，按提示连续输入两次 `yes`。
5. 过期测试：生成普通 token 后让板子保持离线/断电 >10 分钟，再开机。

**预期日志**

```
（未授权）
开机挂卡结束：state=<uninit> 上报=<uninit> free=<n> largest=<n> min=<n>
不格式化：mode=<n> force=0 时间有效=<n> 有到期=0 已用过=0

（授权后）
清单暂无照片，但有未过期的 init token：挂卡检查状态
凭 init_token 格式化（mode=<n> force=0）
格式化完成，init_done=xxxxxxxx

（--force，CRC 损坏卡）
凭 init_token 格式化（mode=<n> force=1）

（token 过期）
（没有 “凭 init_token 格式化”；设备保持 uninit）
```

**通过判据**

- 无 token：绝不格式化，`photo ls` 的 `state=uninit`，卡内容不变。
- 普通 token：空白卡/别的文件系统格式化一次；`init_done` 等于 token 前 8 位；
  `photo ls` 变为 `ready=true state=ok`，服务端随后不再下发该 token。
- 已用 token：格式化后不会再次格式化（串口没有第二条 `格式化完成`）。
- 过期 token：即使设备后来拿到旧清单也不格式化；设备时间未对 SNTP 时同样拒绝。
- CRC 双坏的卡：普通 init 不格式化（`不格式化：... force=0`），只有 `--force`
  的 token 才允许格式化；读失败卡无论 force 都不格式化（见第 18 项）。

---

## 13. 照片上传 → 下载 → 写后校验

**步骤**

1. 用一张带 GPS/EXIF 的手机原图：`stickbox-agent photo add --no-show gps.jpg`。
2. 设备拉清单后自动下载，等待 `photo ls` 的 `已缓存` 增加。
3. 服务端核对最终 JPEG：

   ```bash
   ls -l /opt/stickbox/data/photos/<user_id>/
   sha256sum /opt/stickbox/data/photos/<user_id>/<sha>.jpg
   strings /opt/stickbox/data/photos/<user_id>/<sha>.jpg | grep -Ei 'exif|gps|xmp' || echo 无元数据
   ```

**预期日志**

```
清单更新 plv=<n>，照片 <m> 张
下载 TLS 开始 <sha8>，堆 <n> 最大块 <n>
照片下载完成 <sha8>（<bytes> 字节）
序列重建：照片 <m> 张（在线），当前=board
```

**通过判据**

- 没有 `照片 <sha8> 写入校验失败（sha 不符/写卡错误）`。
- 服务端文件 sha256 与文件名一致、大小等于清单 `size`；`strings` 无 EXIF/GPS/XMP。
- `photo ls` 中该照片 `已缓存`、`bad` 不含它；设备 `have` 增加。
- 原图未留在服务器上（只有缩放后的 JPEG）。

---

## 14. 轮播无黑屏

**步骤**

1. 清单里至少 1 条 board + 3 张照片（默认 20s/8s）。
2. 电视播放 ≥2 轮，观察 board → 照片 → board 的每次切换，同时保持串口日志。

**预期日志**

```
清单更新 plv=<n>，照片 4 张
序列重建：照片 4 张（在线），当前=board
序列重建：照片 4 张（在线），当前=照片
清单未变（304，plv=<n>）
```

**通过判据**

- 每次切换同一台电视**没有出现新的 GET**、没有 `流断开`，画面不黑一下/不暂停。
- 从照片切回 board 时时钟带是新的一分钟，不是旧带。
- 连续 ≥2 轮顺序正确，照片不叠时钟/用量卡片（D10）。

---

## 15. 运行中增 / 删 / 重排

**步骤**

1. 电视正在轮播时上传一张新图：`stickbox-agent photo add new.jpg`。
2. 删除一张：`stickbox-agent photo rm <sha8>`。
3. 重排：用 agent 令牌调用完整清单接口（token 从 agent 配置读取，不打印）：

   ```bash
   TOKEN=$(jq -r .token "$HOME/Library/Application Support/stickbox/agent.json")
   curl -sS -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"entries":[{"kind":"board","theme":"quota","duration_s":20},
                     {"kind":"photo","sha256":"<sha>","duration_s":5}]}' \
     https://mqtt.stickbox.ai/api/playlist
   ```

**预期日志**

```
清单更新 plv=<n+1>，照片 <m+1> 张
序列重建：照片 <m+1> 张（在线），当前=...
回收不在清单里的照片 <sha8>
```

**通过判据**

- 运行中新增：不重启、不断流，新图在下一轮出现；`plv` 前进。
- 删除：只回收被删的那张（日志里点名该 sha 前缀），其他照片不动；`have` 减少。
- 重排后电视按新顺序播放，`duration_s` 变化生效；全程单条流、无黑屏。
- 删除不存在的 sha 返回 404；删除后重传同一文件会重新下载。

---

## 16. 拔卡 / 再插回；无卡冷启动

**步骤**

1. 照片播放中直接拔出 SD 卡，观察电视与串口。
2. 再插回同一张卡，等待自动恢复。
3. 拔卡后重启板子（无卡冷启动）。

**预期日志**

```
（拔卡）
照片连续 3 次读失败，标记 bad（<原因>）
SD 不可用（state=<fault>），本轮只显示看板
（板子不重启，电视回到看板）

（再插回）
SD 处于 fault，重新初始化卡
挂载完成：<卡名>，<n>-bit，容量 <n> MB，状态 <n>，fmt_mode <n>
序列重建：照片 <n> 张（在线），当前=照片

（无卡冷启动）
开机挂卡结束：state=<absent> 上报=<absent> free=<n> largest=<n> min=<n>
```

**通过判据**

- 拔卡：板子不崩、不重启，电视回看板；`photo ls` 的 state 最终为 fault/absent。
- 插回：自动重新挂载并恢复照片轮播，不要求重启。
- 无卡冷启动：看板正常出帧/推流，`photo` 条目被忽略，`photo ls` 显示 absent。

---

## 17. 堆口径：冷启动带缓存 + 加图同步期间

**步骤**

1. 卡里已有 ≥13 张缓存照片，冷启动板子并开串口日志。
2. 稳定后执行 `stickbox-agent photo add 新图.jpg`，让设备下载。
3. 收集以下日志行。

**预期日志**

```
心跳 heap=<n> min=<n>
开机挂卡开始：free=<n> largest=<n> min=<n>
开机挂卡结束：state=<n> 上报=<n> free=<n> largest=<n> min=<n>
heap 对账：free=<n> largest=<n> min=<n>；静态 s_pl=<n> s_buf=<n> ...
下载 TLS 结束，堆 <n> 最大块 <n>
```

**通过判据**

- 冷启动带缓存、以及加图下载同步期间，**所有** `min=` 都 ≥ **40000**
  （本次实测最低 46.7K）。
- 稳态空闲堆仍满足第 2 项的 ≥110K；没有看门狗复位/重启。
- 任一 `min` <40000 即失败，记录发生时的操作（挂卡/下载/取帧并发）。

---

## 18. 卡级损坏只上报、不格式化

**步骤**

1. 插入一张板子读不了的卡（初始化超时/CRC 错误），**先不做任何 init**。
2. 再执行 `photo init`（普通与 `--force` 各一次），观察是否写卡。
3. 保持板子联网 ≥2 个心跳，查看 `photo ls`。

**预期日志**

```
SD 初始化失败：<err>
SD 不可用（state=<fault>），本轮只显示看板
（没有 “凭 init_token 格式化”，也没有 “格式化完成”）
```

**通过判据**

- 无论有没有 token、是否 `--force`，读不了的卡都**绝不**格式化。
- `photo ls` 持续显示 `state=fault`；看板与电视推流正常，板子不重启。
- 卡恢复（电脑重新格式化或换卡）后再 init 可正常使用。

---

## 19. 发布记录

每个版本跑完填一行，附日志路径。

| 日期 | 固件版本 | commit | 板子/LCD | 电视 | 1 冷启动 | 2 内存 | 3 停服 | 4 解绑 | 5 重配 | 6 配额 | 7 回滚 | 8 地址 | 10 签名升级 | 11 拒绝未签名 | 12 格式化 | 13 下载校验 | 14 轮播 | 15 增删排 | 16 拔插卡 | 17 照片堆 | 18 坏卡 | 结论 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
|  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |

> 第 12–18 项只适用于带 SD 槽的板子；没有 SD 槽的构建记 N/A 并注明。

建议的一次完整跑顺序（减少等待）：

1. 第 8 项（离线，先卡住固件地址）
2. 第 9 节开始前清理 → 第 1 项冷启动
3. 第 2 项内存（10 分钟；期间可做第 8 项复核）
4. 第 3 项停服 90 秒
5. 第 4 项解绑 → 第 5 项重配
6. 第 6 项配额（约 5～10 分钟）
7. 第 7 项 OTA 回滚（约 25 分钟，含两次 10 分钟检查窗口）
8. 第 10、11 项签名 OTA 验收（各约 15 分钟；需要签名/未签名两个产物）
9. 照片：第 12 项格式化 → 第 13 项下载校验 → 第 14 项轮播 →
   第 15 项运行中增删排 → 第 16 项拔插卡 → 第 17 项堆 → 第 18 项坏卡
   （需要 SD 卡；每项 5–15 分钟）
10. 第 9 节恢复现场
