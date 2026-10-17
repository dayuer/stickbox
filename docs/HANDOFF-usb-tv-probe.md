# 虚拟 U 盘探针 — 移交文档

把 ESP32-S3-USB-OTG 插进电视（目标机型：三星 Tizen）的 USB 口，伪装成一支 U 盘，
用来判断「USB 信息屏」这个产品想法成不成立。

**这是一个一次性诊断工具，不是产品原型。**它的产出是三个问题的答案，不是功能。
跑完实验就该删掉或者改写，不要往上堆功能。

本文只写**结论和已验证的事实**。走过的弯路写在「踩过的坑」一节，是为了让接手的人
不要再走一遍。

工程位置：`probe/usb-tv/`，独立 IDF 工程，与主工程零耦合。

---

## 1. 它要回答什么

| | 问题 | 为什么它是命门 |
|---|---|---|
| Q1 | 电视认不认一支**合成出来**的 FAT32 U 盘？ | 不认则一切结束 |
| **Q2** | **盘上内容变了，怎样才能让电视重新扫描？** | **这个答案决定产品形态** |
| Q3 | 它按什么顺序读哪些扇区？ | 决定能不能现场渲染、吞吐上限在哪 |

Q2 是重点。电视会读一次 FAT、建缩略图索引，之后文件变了它**不一定重读**。
如果没有任何手段能让它重扫，这个盘就只能提供开机时那一份静态内容，
「信息屏」退化成「开机画面」，整个想法不成立。

---

## 2. 架构

```
Wi-Fi ──HTTP──> ESP32 ──USB MSC（只读）──> 电视
  ↑               │
手机控制面板     合成扇区：主机读哪个扇区就现场算一个
```

职责划分：

| 部件 | 干什么 | 明确不干什么 |
|---|---|---|
| `vdisk.c` | 按 LBA 现场生成扇区；管幻灯片切换 | **不在 RAM 里放磁盘镜像**、不碰 USB |
| `usbdev.c` | MSC 设备、描述符、PHY、三种重扫手段 | 不认识 FAT |
| `httpd.c` | 手机控制面板 + 诊断输出 | 不碰 USB 状态机（丢给 worker 任务） |

**为什么是合成扇区**：这块板子（ESP32-S3-MINI-1-N8）没有 PSRAM，空闲堆只有
72~120KB，装不下任何真实大小的 FAT 卷——FAT32 的最小合法卷就要 32MiB 以上。
而一张 64MiB 的盘里 99.9% 的扇区是零，根本不需要存。图片用 `EMBED_FILES` 进 flash
（const，flash 映射直读），同样不占堆。

**为什么控制面板是网页而不是串口命令**：测试时人站在电视前面，不是坐在电脑前。

---

## 3. 为什么绕过 esp_tinyusb（最重要的一条）

**不能用 `espressif/esp_tinyusb` 做自定义 MSC。** 这不是风格选择，是硬约束：

- 它的 `device/esp_tinyusb/tinyusb_msc.c` **无条件定义了全部 8 个 `tud_msc_*` 回调**
  （`read10` / `write10` / `inquiry` / `test_unit_ready` / `capacity` / `start_stop` /
  `scsi` / `get_maxlun`），强符号，**没有 `__attribute__((weak))`**。
- 它的 `include/tusb_config.h` 里是 `#define CFG_TUD_MSC CONFIG_TINYUSB_MSC_ENABLED`，
  而 `CMakeLists.txt` 里 `tinyusb_msc.c` 就编在 `if(CONFIG_TINYUSB_MSC_ENABLED)` 里面。

于是：**想开 MSC 类 → 必须开那个 Kconfig → 必然编进它的存储层 → 和自己的回调撞符号。**
Kconfig 的 MSC 菜单里只有 `TINYUSB_MSC_ENABLED` / `MSC_BUFSIZE` / `MSC_MOUNT_PATH`
三项，**没有任何 raw/custom 逃生开关**。

而它的存储层只支持 wear-levelling 分区和 SDMMC（`storage_blockdev.c` 要 IDF 6.0.4+
的 `esp_blockdev`），都落不到「合成扇区」上。

**结论**：直接依赖底层 `espressif/tinyusb`，自带 `tusb_config.h`、描述符、
PHY 初始化、`tud_task()` 任务。代价约 80 行胶水。

编译后必须验证符号只有一份：

```sh
nm build/usb_tv_probe.elf | grep tud_msc_read10_cb   # 只应有一行
```

### 顺带验证到的 esp_tinyusb 2.x API（将来若要用它）

`tinyusb_config_t` 是**嵌套**的，不是扁平的：

```c
tinyusb_config_t cfg = TINYUSB_DEFAULT_CONFIG();
cfg.descriptor.device            = &dev_desc;
cfg.descriptor.full_speed_config = cfg_desc;
cfg.descriptor.string            = str_arr;      /* const char ** */
cfg.descriptor.string_count      = N;
cfg.phy.self_powered             = false;        /* 不在顶层！ */
cfg.phy.vbus_monitor_io          = -1;
```

顶层字段是 `port / phy / task / descriptor / pm_lock_enable / event_cb / event_arg`。
`self_powered` 和 `vbus_monitor_io` 在 `phy` 里面——README 的措辞会让人以为在顶层，
那是错的。

---

## 4. 磁盘几何（别改，除非重做这段算术）

```
扇区 512B，整盘 131072 扇区 = 64 MiB
LBA 0        MBR，一个分区，类型 0x0C (FAT32 LBA)
LBA 2048     分区起点（真 U 盘的标准对齐），长 129024 扇区

卷内（相对分区起点）：
  0      BPB            6     BPB 备份
  1      FSInfo         7     FSInfo 备份
  32     FAT1 (1008)    1040  FAT2 (1008)
  2048   数据区 = 簇 2

数据扇区 = 129024 - 32 - 2*1008 = 126976 → 126976 个簇（每簇 1 扇区）
FAT 容量 = 1008 * 128 = 129024 项 ≥ 126978 ✓
```

**簇数必须 ≥ 65525。**这是 FAT32 的硬门槛，低于它主机会按 FAT16 解析然后失败。
这是改几何时最容易踩的雷。

簇分配：

```
2              根目录
3              /DCIM
4    .. 2051   /SLIDE.JPG         （窗口 2048 簇 = 1 MiB 上限）
2052 .. 4099   /DCIM/SLIDE.JPG    （同一份数据的第二条链）
```

**为什么带 MBR**：有些电视不认没有分区表的「超级软盘」布局。带 MBR 长得和真 U 盘一样。

**为什么放两份**：不确定电视的照片浏览器扫根目录还是扫 `DCIM`。两边都放，
哪边被读到直接反映在 `/trace` 里。两条链各自独立（**不共享簇**），文件系统完全合法。

**为什么三张图字节数差很多**（18KB / 367KB / 291KB）：文件长度一变，FAT 链长和
目录项里的 size 字段就都变了，于是「电视有没有重读 FAT」变得可观测。
**这是实验的前提，换图时必须保持长度差异。**

---

## 5. 三种重扫手段

| | 手段 | 机制 | 预期 |
|---|---|---|---|
| **A** | 热换 | 直接改内容，不动 USB | 大概率失败。但要亲眼确认一次——万一成了，方案瞬间简单十倍 |
| **B** | 介质弹出 | `test_unit_ready` 报 `NOT READY / 0x3A MEDIUM NOT PRESENT`，装回后报一次 `UNIT ATTENTION / 0x28`（介质可能已更换） | **最该成功的那个。** 读卡器换卡就是这么干的，而且 USB 连接本身不断，对电视最温和 |
| **C** | USB 软拔插 | `tud_disconnect()` / `tud_connect()` | 整条总线重新枚举，最彻底 |
| **C+** | C + 可变序列号 | 每次枚举换一个序列号字符串 | 最后一招。很多主机按 `(VID,PID,序列号)` 缓存卷信息 |

`0x28` 那个 UNIT ATTENTION 是 SCSI 里「介质换了，请重读」的**标准**信号，
比单纯 toggle not-ready 正确得多。如果 B 行，整件事很优雅。

---

## 6. 已验证的硬事实（这些是资产）

### 6.1 FAT32 布局已主机侧验证，141 项全绿

`probe/usb-tv/test/run.sh` 把 `main/vdisk.c` **原封不动**编到 x86 上，导出镜像前
8300 个扇区，交给 `test/validate.py`——一个**独立按 FAT32 规范写的**解析器。

> 关键设计：validator 刻意**不引用 `vdisk.c` 里的任何常量**，所有几何都从 MBR/BPB
> 现读现算。否则就是把同一个错误验两遍。

终局判据：从簇链里把 `/SLIDE.JPG` 和 `/DCIM/SLIDE.JPG` 的字节抠出来，和
`assets/slideN.jpg` **逐字节比对**。一致 ⇒ BPB 几何、FAT 链接、目录项簇号、
数据区落位全对。

三张图各一遍，覆盖到：

- MBR 分区表、类型 0x0C、2048 对齐，另外三个表项为空
- BPB 的 15 个字段、`FilSysType`、扩展引导签名 0x29
- 簇数 126976 ≥ 65525
- 备份引导扇区与主引导扇区逐字节一致、FSInfo 三个签名
- FAT2 与 FAT1 完全一致
- `.` / `..` 项，`..` 的簇号为 0（FAT32 里父为根时的规定）
- **大图 717 簇的链跨越 6 个 FAT 扇区仍然正确**（`gen_fat` 最容易错的地方）
- 两个副本的簇链完全不重叠

**改 `vdisk.c` 之后必须重跑这个。**它不需要 ESP-IDF 也不需要板子。

### 6.2 三星电视的 USB 口吃什么

| | 支持 | 说明 |
|---|---|---|
| U 盘 (MSC) | ✅ | Media 播放器读照片/视频/音乐 |
| 键鼠 (HID) | ✅ 但受限 | 官方支持页：2014 年 H 系列以后的多数机型 |
| USB 网卡 (RNDIS/NCM) | ❌ | Tizen 不加载驱动 |
| USB 声卡 | ❌ | |

HID 的限制：只认**标准 HID**；键盘能做菜单导航和浏览器输入，**鼠标基本只在浏览器里有用**。
2013-2014 的 F/H 系列只认三星自家键盘。用户报告互相矛盾（同型号有人说行有人说不行）。

**MSC + HID 是一个配置两个接口，TinyUSB 原生支持**，将来可以同时在。
但注意 **S3 只有一个 OTG 控制器**，device 模式下不能同时当 host。

### 6.3 Tizen 的控制通道在网络上，不在 USB 上

- 端口 **8001**（不加密，老机型）/ **8002**（TLS，新机型必须）
- 路径 `samsung.remote.control`，设备名 base64 编码
- 首次连接电视弹 Allow/Deny，同意后返回 token。**3 秒内收不到
  `ms.channel.connect` 就说明电视在等你按允许**
- 发键：`method: ms.remote.control`、`Cmd: Click`、`DataOfCmd: KEY_MUTE`、
  `TypeOfRemote: SendRemoteKey`
- ⚠️ **三星从未官方文档化或支持过这个协议**，固件更新可能改行为；有用户报告
  **token 会在会话之间变化**

**分工**：发遥控键码走 Wi-Fi（可靠、是事实标准路径），USB 只做网络做不到的事——
动态内容（虚拟 U 盘）和配网向导阶段的输入（HID）。

### 6.4 数字

| | |
|---|---|
| MSC 吞吐 | ESP-FAQ 对 TinyUSB 的预期值 **6.4 Mbps** —— 放照片够，放视频不够 |
| `CFG_TUD_MSC_EP_BUFSIZE` | 4096（一次 8 扇区）换吞吐，代价 4KB 常驻。**没 PSRAM，别再加** |
| 单文件上限 | 1 MiB（`FILE_MAX_CLUS = 2048`） |
| 电视 USB 待机供电 | 三星官方：关机后 **1~2 分钟**断电。但大量用户报告新机型待机一直带电，且**没有开关可关**。要「关机也在线」就不能只靠 USB 取电 |
| 电流 | 没找到 500mA 的官方说法；有用户报告某型号 5V 1A。**查自己那台的手册** |

---

## 7. 没验证的（风险清单）

**`usbdev.c` 整个 USB 层从未编译过**——容器里没有 ESP-IDF。三处标了 `[待验证]`：

| 位置 | 不确定的 | 兜底 |
|---|---|---|
| `usbdev_start()` | `usb_phy_config_t` 字段名、`usb_new_phy()` | 照真实结构体改 |
| `usbdev_replug()` | `tud_disconnect()` / `tud_connect()` | 没有就删掉实验 C，只做 A/B |
| MSC 回调 | `tud_msc_set_sense()` 参数顺序、`SCSI_SENSE_*` 枚举名 | ASC/ASCQ 已用字面量避开，只剩 sense key |

`tools/check-api.sh` 在 `idf.py reconfigure` 之后一键核对这三处，**顺带检查有没有
误把 `esp_tinyusb` 拉进依赖**。全部 ✓ 再烧。

另外：`espressif/tinyusb` 的版本在 `idf_component.yml` 里是 `"*"`。
**第一次编过之后请固定成实际解析到的版本。**

---

## 8. 测试协议

接线：**电视 USB ← DEV 口**，**电脑 ← Micro-USB 调试口**（两个同时插）。

控制台走 UART0，因为 OTG 外设占了 GPIO19/20。所以这个固件**不会吃掉串口**——
不像主工程的打印机固件（切 host 模式后 DEV 口消失，每次烧录都要按 BOOT→RST）。
走 Micro-USB 口烧录不需要那套动作。

> ⚠️ **安全要点**：`board_usb_device_mode()` 把 `DEV_VBUS_EN` 和 `BOOST_EN` 都置 0，
> 绝不往 DEV 口灌 5V。VBUS 由电视提供，两个源对顶会出事。**别改那几行的值。**

手机打开控制面板，固定三步，每种手段走一遍：

1. **① 计数归零**
2. 点一种换片方式
3. 看 `fat` 和 `dir` 计数**有没有从 0 涨起来**

`fat`/`dir` 涨了 = 真的重读了文件系统。只有 `data` 涨 = 在用缓存的目录结构。

`/trace` 看读访问顺序：读不读 MBR、认不认备份引导扇区、扫根目录还是 `DCIM`、
一次读几个扇区。

---

## 9. 决策树：结果 → 下一步

| 结果 | 下一步 |
|---|---|
| **B 或 C 有效** | 做内容通道：换成 `esp_new_jpeg` 现场渲染 + Wi-Fi 拉数据。先确认它的 API 能不能**逐块喂**——240×240 RGB565 = 115KB，一次吃整张图在这块板子上放不下 |
| **只有 C+ 有效** | 先量重新枚举对电视界面的干扰（会不会跳回信号源列表），再定刷新频率。大概率只能做低频刷新 |
| **全都无效** | **这条路到此为止。** 退回 Tizen WebSocket（8002）做控制，USB 只留着当 HID 键盘。别硬写下去 |
| Q1 就失败 | 先看 `/trace`：`bpb` 有读但 `fat`/`dir` 没读 ⇒ 它拒绝了 BPB；一次读都没有 ⇒ 枚举阶段就失败，与文件系统无关，查串口的 `tud_mount_cb` |

---

## 10. 踩过的坑

1. **以为 `esp_tinyusb` 能做自定义 MSC。** 查了源码才发现符号冲突无解（见第 3 节）。
   如果一上手就写，会在链接阶段撞墙，然后以为是自己写错了。
2. **以为 `tinyusb_config_t` 是扁平结构**（`.device_descriptor` / `.string_descriptor` /
   `.configuration_descriptor`）。实际是嵌套的 `.descriptor.*`，`self_powered` 在 `.phy` 里。
   查证之前别照记忆写。
3. **差点做成「超级软盘」**（无 MBR）。真 U 盘都有分区表，有些电视不认裸 FAT 卷。
4. **差点让两个副本共享簇链。** 读起来能工作，但文件系统不合法，电视的 FAT 驱动
   如果做校验可能整卷拒绝。两条独立链的内存开销是零（数据都从同一份 flash 读），
   没有理由省。
5. **先想的是 Wi-Fi 投屏，不是 USB。** 结论是 ESP32-S3 **没有硬件视频编码器**
   （连 JPEG 硬编码都没有；摄像头项目能出 JPEG 是 OV2640 传感器自己压的），
   1280×720 RGB565 单张 framebuffer 1.84MB 对 120KB 堆差 15 倍。真要编码得上
   ESP32-P4（硬件 H.264、1080p30），但 P4 没有 Wi-Fi。
   **USB 这条路绕开了编码问题**——电视自己解 JPEG。这是它比投屏优越的根本原因。
6. **三种重扫手段一开始只想做软拔插。** 加上 SCSI 的 `UNIT ATTENTION 0x28` 之后
   才有了「最温和且标准」的那个选项。做实验要把阶梯铺满，不然只能知道
   「最重的手段行不行」，不知道「最轻的手段够不够」。

---

## 11. 文件导览

```
probe/usb-tv/
├── main/
│   ├── vdisk.c/.h        合成磁盘：几何推导在文件头，必读
│   ├── usbdev.c/.h       MSC 设备 + 三种重扫手段（三处 [待验证] 在这）
│   ├── tusb_config.h     绕过 esp_tinyusb 的关键
│   ├── httpd.c/.h        手机控制面板
│   └── main.c            board_usb_device_mode() 有 VBUS 安全要点
├── assets/slide[123].jpg 三张图，字节数刻意差很多
├── test/
│   ├── run.sh            主机侧校验入口，改 vdisk.c 后必跑
│   ├── validate.py       独立 FAT32 解析器（不引用 vdisk.c 的常量）
│   ├── harness.c         把 vdisk.c 编到 x86 导出镜像
│   └── stubs/            esp_log.h / esp_timer.h 的主机桩
├── tools/
│   ├── check-api.sh      核对三处待验证 API + 查 esp_tinyusb 污染
│   └── make_slides.sh    重新生成测试图（ImageMagick）
└── README.md             操作手册；本文是设计与决策记录
```

分工：**README 是「怎么用」，本文是「为什么这么做、什么已验证、结果怎么判读」。**
