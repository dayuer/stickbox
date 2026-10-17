# StickBox 固件（ESP32-S3）

一块 ESP32-S3（8MB flash）插在电视 USB 口取电，把 Claude 用量看板
推给电视，同时在板载 LCD 上显示状态。固件负责：Wi-Fi 配网、设备注册/
配对码、MQTT（用量与配对通知）、DLNA 1080p MJPEG 推流、看板渲染。

- 目标：`esp32s3`，ESP-IDF **5.5**
- USB：ESP32-S3 内置 USB-Serial/JTAG（`/dev/cu.usbmodem*`）
- 分区表：`partitions.csv`
- 接口约定：`docs/API.md`；帧接口：`docs/FRAME.md`；部署：`server/DEPLOY.md`

---

## 1. 编译

```bash
source ~/esp/esp-idf-v5.5/export.sh
cd rom

cp main/cloud_creds.h.example main/cloud_creds.h   # 填自己的地址
idf.py build
```

`cloud_creds.h` 不入库，只需要两个地址：

```c
#define MQTT_URI "mqtts://mqtt.stickbox.ai:8883"
#define API_BASE "https://mqtt.stickbox.ai/api"
```

- **设备密钥不在这里**：设备首次联网自己调 `/api/device/register` 领，
  配对后写进 NVS `cloud/devkey`。
- 本机联调连自签证书服务端时，可以临时加
  `#define STICKBOX_INSECURE_TLS 1`；它会在编译时打 `#warning`，**正式固件不要定义**。
- 服务器地址进配网页是 T11 的工作；在那之前地址是编译期常量，换服务器要重编。

发布构建（签名、OTA 产物）由 T15b 的 release 流水线负责；合入后见
`docs/RELEASE.md`。不要用开发机的 `sdkconfig`。

---

## 2. 烧录

```bash
idf.py -p /dev/cu.usbmodem1301 --after watchdog_reset flash
```

**`--after watchdog_reset` 不能省。** 这块板子的 USB-JTAG 在烧录后默认的
hard reset 有时不会把 app 放起来，表现为烧完没反应、日志一片空白；
`watchdog_reset` 能稳定启动。

首次烧录或换了分区表时：

```bash
idf.py -p /dev/cu.usbmodem1301 --after watchdog_reset erase-flash flash
```

### 停住板子（不跑 app）

做电视/DLNA 实验或不想让板子去推流时：

```bash
esptool.py -p /dev/cu.usbmodem1301 --after no_reset chip_id
```

输出 `Staying in bootloader.` 就是停住了。**注意 `--after` 默认是
`hard_reset`**，不带它会把 app 放起来；必须显式 `no_reset`。

恢复：

```bash
esptool.py -p /dev/cu.usbmodem1301 --after watchdog_reset chip_id
```

或者直接拔插 USB。

---

## 3. 串口日志

用仓库里的只读工具，不要直接 `idf.py monitor` / `screen` / 裸 pyserial：

```bash
python3 rom/tools/serial_log.py /dev/cu.usbmodem1301 115200
```

原因：这块板的 DTR/RTS 虚拟捆绑到 GPIO0/EN。pyserial 打开端口时默认把
两条线拉高，等于「按住 BOOT 再按 RST」——芯片被按进下载模式且复位不出来。
`serial_log.py` 在 `open()` 之前先把 DTR/RTS 置低，只读不写。

- 想看开机日志：工具跑起来后手动点一下 RST。
- 打开串口本身仍可能导致一次 `USB_UART_CHIP_RESET`；如果板子正在推流，
  尽量在空闲时看日志。
- 需要 pyserial：`pip install pyserial`。

---

## 4. 配网

1. 首次上电（或 NVS 里没有 Wi-Fi）时，板子开热点
   `StickBox-Setup-XXXX`（后四位是 MAC），LCD 显示热点名和
   `192.168.4.1`。
2. 手机连这个热点，浏览器打开 `http://192.168.4.1/`，选 2.4GHz Wi-Fi、
   填密码。页面会先测试连接，成功后写 NVS 并重启。
3. 连不上常见原因：选了 5GHz 网络（ESP32 只支持 2.4GHz）、密码错、
   路由器开了客户端隔离。

**重新配网**：开机时按住 MENU 键（GPIO14）。会同时清空 Wi-Fi 配置和
设备密钥，重启后重新出配对码。

---

## 5. 配对

配网成功后设备会：

1. `POST /api/device/register {dev}` 领设备密钥 + 6 位配对码；
2. 把配对码显示在电视和 LCD 上（10 分钟有效，过期自动换码）；
3. 连 MQTT，订阅自己的 `printer/{dev}/usage` 和 `printer/{dev}/pair`。

在电脑上运行 `stickbox-agent pair 123456`，或在任意机器上：

```bash
curl -sS -X POST https://mqtt.stickbox.ai/api/agent/pair \
  -H 'Content-Type: application/json' -d '{"code":"123456","name":"my-mac"}'
```

配对成功后电视切到看板，板载 LCD 显示「已配对」。解绑用
`POST /api/agent/unpair {dev}`；设备收到通知会清密钥、重启、重新出码。

完整字段、错误码、限流见 [`docs/API.md`](../docs/API.md)。

---

## 6. 工具

| 工具 | 用途 |
|---|---|
| `tools/serial_log.py` | 只读串口日志（DTR/RTS 安全） |
| `tools/render_pages.py` | 重新生成 `main/assets/page{1,2,3}.jpg` 预置页（需要 Pillow） |
| `tools/gen_web_manifest.py` | 从 `flasher_args.json` 生成烧录页 manifest（T15a） |

发布构建脚本（`release_build.sh`、`check_app_version.py`、
`make_ota_manifest.py`）在 T15b 合入后位于 `tools/`。

---

## 7. 故障排查

### 烧完没反应 / 日志空白

- 烧录时漏了 `--after watchdog_reset`；重新烧一次。
- 板子停在下载模式：`esptool.py -p PORT --after no_reset chip_id` 会停住，
  用 `--after watchdog_reset` 恢复或拔插 USB。
- 串口工具把 DTR/RTS 拉高导致进了下载模式：改用 `serial_log.py`。

### USB 口消失 / 反复重连

- 换线、换直连 USB 口（不要用无源 hub）。
- USB-JTAG 在复位时可能短暂消失，属正常；持续消失时拔插。
- 烧录/复位期间不要同时开多个串口工具。

### TLS 握手失败

- 正式固件用系统 CA bundle 校验 `mqtt.stickbox.ai`；确认服务器证书链完整、
  SNI 正确、系统时间正确（板子会 SNTP 对时）。
- 自签证书联调：临时定义 `STICKBOX_INSECURE_TLS`，它只跳过校验，不解决
  地址/端口/证书 CN 错误；正式固件必须去掉。
- 服务端 HTTPS 走 nginx 443 反代，MQTT 直连 8883，见 `server/DEPLOY.md`。

### 配对码一直不出来

- 看串口日志里 `/api/device/register` 的 HTTP 状态；401/404 多半是
  `API_BASE` 填错或 nginx 没转发 `/api/`。
- 429 是限流（每 dev 10 分钟 5 次注册）；等一会儿。
- 配对码 10 分钟过期会自动换码；电视/LCD 上的码以最新一次为准。

### 电视不出画 / 起播慢

见 [`docs/HANDOFF-tv-startup.md`](../docs/HANDOFF-tv-startup.md)：推荐
开机突发 8 帧 + 2fps；DLNA 实验记录在
[`docs/HANDOFF-usb-tv-probe.md`](../docs/HANDOFF-usb-tv-probe.md)。

---

## 8. 不要提交的东西

`main/cloud_creds.h`、`sdkconfig`、`sdkconfig.old`、`build/`、
`managed_components/` 都不入库（见根目录 `.gitignore`）。签名私钥
`ota_key.pem` 也绝不能入库——本地构建后确认 `git status` 干净。
发布密钥的生成与保管见 `docs/RELEASE.md`（T15b 合入后）。
