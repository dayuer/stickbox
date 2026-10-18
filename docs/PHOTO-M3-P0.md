# M3 P0：照片播放（stream.c 第四种模式）

状态：**设计草案，等 USB-TV 确认后写代码。** 分支 `task/P2-m3-play`。
基线：M2（清单/下载/互斥/status）已上板；M3 只做「按播放序列把 SD 上的
JPEG 从 `/live.avi` 发出去」，不改下载协议。

## 0. 范围与不变量

- `/live.avi` 不换 URL、不重发 AVI 头、不断流；电视端只是在下一个 `00dc`
  块看到不同的 JPEG。切换只在**帧边界**发生（现有 `client_task` 里那个
  「每帧只看一次共享状态」的临界区）。
- 看板（帧包 + 时钟带）仍是主内容；照片是序列里的条目。当前服务端只发
  「1 条 board（位置 0）+ 照片」，状态机按**任意 board/photo 交替**实现，
  以后放开多 board 不用重写。
- 没卡 / 卡 uninit / 卡 fault：序列里只保留 board，行为和现在逐字节一致。
- 照片纯显示：不叠时钟带、不碰 `s_band`、不建段表、不碰 `s_fp_hold`。

## 1. 帧率

- 沿用 `CONFIG_STICKBOX_DLNA_FPS`，当前 **2fps**；在 `photo_play.h` 里用
  `PHOTO_PLAY_FPS` 引用它，不再散落常数。
- 先按 2fps 设计。1fps 只需改常量 + 重连（AVI 头的
  `dwMicroSecPerFrame`、`strh.rate` 从 `s_fps` 取）。
- **0.5fps 不是改常量就完**：`s_fps` 现在是整数，`1000000/s_fps`、
  `strh.rate` 都会坏。固件改用 `PHOTO_PLAY_FRAME_US` 表示帧间隔，
  AVI 头在 0.5fps 时写 `rate=1 / scale=2`；这是电视测出 0.5fps 稳定后
  的一段小改动，先不阻塞 2fps 版本。
- 序列推进用**墙钟**（`esp_timer_get_time()`），不是数帧。帧率只决定
  切换的粒度：2fps 下切换最多晚 0.5s，1fps 最多 1s，0.5fps 最多 2s。
  `duration_s * fps` 只作为一致性/自检值，不作为计时依据，这样某台电视
  卡顿不会拖慢所有连接的序列。

## 2. 序列、切换点与停留时长

### 2.1 序列来源

- **在线**：用 M2 已解析的清单。`photos[]` 按数组顺序（`order` 0..N-1
  是权威），board 条目取第一条（服务端保证只有一条、在位置 0）。
  M2 的清单结构要补两个字段：`board_duration_s`、`board_theme`。
- **离线/刚开机**：用 SD 索引。board 用 flash 里当前激活的帧包，默认
  停留 20s；照片按索引里的 `order` 排序（相同 order 按槽号），
  `duration_s==0` 用默认 8s。开机 `photo_sync_boot_mount()` 只挂卡、
  **不格式化**（没有 token 绝不写卡），挂好后 bump 代数，播放器就能
  在断网时从索引起播。
- 每次清单更新 / 下载完成 / GC 后重算序列；**当前照片仍在新序列且校验
  有效就保持**（不从头切），否则在下一帧边界切到序列第一项（board）。
- `photo_sync` 暴露 `photo_sync_playlist_gen()`（每次应用清单递增）和
  `photo_sync_playlist_visit(cb, arg)`（在任务里逐项回调，避免把 8KB
  清单复制到 4KB 调度栈上）；`photo_play` 用静态序列数组（≤201 项）。

### 2.2 时间

- 每个条目记录 `start_us` 和 `duration_s`；到点后选**下一个可播项**
  （缺文件、bad、超过 AVI 单帧上限的整条跳过，不占停留时间）。
- board 条目：`duration_s` 来自清单（默认 20s）；照片条目：5–600s
  （服务端约束），索引里的 0 按 8s。
- 切换发生在下一次帧边界：调度任务只改共享快照，`client_task` 在帧开头
  取快照。因此实际停留 = `duration_s ± 一个帧间隔`。
- 多台电视共享同一个全局序列和时钟：它们看到同一张照片，各自按自己的
  连接节奏取帧；后连上的电视从当前条目的当前位置开始，不补播。

### 2.3 board ↔ photo

- board 项：`s_mode=BOARD`，走现有帧包 + 时钟带路径。从照片切回 board 时
  强制重发一版当前时钟带（把 `s_last_min` 置 -1 或调一次 `attach_active`），
  否则电视上会先出现一条过期时钟带。
- photo 项：`s_mode=PHOTO`，本帧只发该 JPEG；时钟带任务照常跑，但
  `s_mode` 不是 BOARD 时它发布的带不会被读取。
- 服务端当前只允许 1 条 board（位置 0）；状态机仍按通用序列实现，
  将来多条 board 只受「flash 里能驻留几个主题」限制，不在 M3 范围。

## 3. 互斥与延迟预算

M2 的 `photo_io` 一把锁在 M3 拆成两把，锁序固定 **net → store**，禁止反向：

| 锁 | 保护 | 持有者 |
|---|---|---|
| `photo_io_net` | TLS/HTTPS 会话、堆峰值 | 取帧、清单拉取、下载的 HTTP 段 |
| `photo_io_store` | SD/photostore 的共享 4KB 缓冲与索引 | 下载写卡、GC/元数据、格式化、播放读卡 |

- **播放只拿 store，不拿 net**：照片播放不需要 TLS，因此**取帧永远不会
  被照片播放挡住**。这是「不让看板帧长期取不到」的根本手段。
- 取帧只拿 net：被下载挡住 ≤ 一次 4KB 读（下载每 4KB 检查
  `framefetch_busy()`，有取帧就断流放锁、保留 Range），量级 20–50ms。
- **清单拉取的上界**：开始前若 `framefetch_busy()` 直接让路；持锁后每读
  一块都检查，取帧在等且已持锁超过 **500ms** 就断流、放锁、1 秒后重试。
  加上最多一次 HTTPS 连接阶段（正常 <1s），看板帧被清单拉取挡住的上界
  约 **1.5s**；超过这个值只可能是 TCP/TLS 超时，那时清单本来也失败了。
- 播放被挡住的最坏情况：下载写一个 4KB 块（几 ms）、GC 扫 256 项
  （约 20–40ms）、格式化（只在初始化/授权时发生，秒级；期间序列强制
  board）。取帧 TLS 对播放 **0 延迟**。
- 播放读卡粒度 8KB：拿 store → `photostore_read` → 放 store → `send_all`。
  `send` 不持锁，电视卡住最多阻塞自己的连接任务（SO_SNDTIMEO 15s），
  不影响取帧和下载。
- 堆：每个连接任务 8KB 读缓冲（电视通常 1 条连接）；调度任务约 1KB；
  无新增 TLS。下载的 60KB 堆门槛不变。

## 4. stream.c 状态机

新增 `s_mode`（与 `s_fp_mode` 分离）：

```
                 pair_pending / 无 board
   ┌───────────────────────────────────────┐
   v                                       │
OVERRIDE（配对页/状态页，现有逻辑）         │
   │ 配对完成且 board 可用                  │
   v                                       │
BOARD ──序列到点，下一项是 photo──> PHOTO ──序列到点/读失败──┐
   ^                                       │                │
   └───────────────────────────────────────┴────────────────┘
```

- `s_fp_mode` 仍表示「flash 里有可用帧包」；`stream_use_framepack()` 只更新
  帧包快照，**不再强行把显示模式切到 board**。取帧在照片播放期间完成时，
  新的 board 字节就绪，但画面保持照片，等序列回到 board 项再用。
- `client_task` 帧边界快照：`{s_mode, board 快照, photo 快照}`。PHOTO 分支：
  1. 拿 store 锁，`photostore_get(current_sha)` 校验 start/size 仍有效；
  2. 有效则写 `00dc` 头（长度 = 索引 size），按 8KB 读一块、放锁、`send_all`；
  3. 奇数长度补 1 字节 pad；整张发完，等下一拍。
- 帧段编排（`fp_frame_segments`）、`s_band`、`s_fp_hold` 在 PHOTO 分支
  完全不参与。
- 单帧上限：照片 `size + 8` 必须 ≤ AVI 头里的 `maxframe`
  （`FP_MAX_PACK + BAND_CAP + 8`，实测约 275KB）。服务端压到 ≤250KB，
  正常都过；超限的条目标 bad 并跳过，绝不发一个电视预分配缓冲装不下的帧。

### 4.1 播放期间的数据生命周期（必须做，否则会发错图）

- 帧边界快照照片时，对当前 sha 记一个 **hold**（每连接一份），本帧发完
  或连接退出时释放；类似 `s_fp_hold` 保护 flash 映射。
- M2 的 GC 回调在 `photostore_remove` 前先问 `photo_play_is_held(sha)`：
  被持有的条目不删，留到下一次 GC；序列已经不再播它，但不影响正在收尾
  的连接。否则 GC 释放的空间可能被下一张照片复用，正在发的 JPEG 会
  半新半旧。
- 格式化会清空整卡：只在卡不是 READY 时发生；`photo_sync` 在格式化前把
  状态置为非 READY，调度器看到后立刻切 board，并等所有 photo hold 归零
  （最多等一个帧发送/连接超时）再动卡。

## 5. 掉卡 / 读失败 / 回退

- 没卡、uninit、fault：`photo_sd_store()` 为 NULL，序列只含 board，
  `s_mode=BOARD`；配对页/状态页优先级不变。
- 照片条目在序列里但文件不存在（被 GC/删除）：条目直接跳过，下一帧边界
  切到下一项；不会发空帧。
- **读失败分两种**：
  - 在写 `00dc` 头之前就失败（第一块读不出来）：本帧改发 board，不断流；
  - 已经写了头、发到一半失败：**无法修补一个短块**，唯一安全的动作是
    关闭这条连接（`break`）。电视会停在最后一帧，等现有 supervisor 的
    idle 重推逻辑重新 SetAVTransportURI（最长一个 repush 周期）；新连接
    拿到新的 AVI 头和 board。绝不能补零/少发字节，那会让电视错位并可能
    一直花屏。
- 读失败/校验失败的照片：标记 suspect，交给 M2 的照片任务做一次全量
  sha 重校验；不符就删掉索引并重新下载。重下完成前序列跳过它。
- **连续失败计数**：同一 sha 连续 3 次读失败（首块失败或半张失败）就
  标 bad、进 `status.photo.bad` 并跳过，不再无限重试；只有一整张成功
  发完才清零计数。跨照片/跨连接按 sha 分别计。
- 卡被拔掉：上述读失败路径生效；M2 在下次清单/下载时检测到 mount/read
  失败，把 `state` 置 fault；status 心跳照常报 `fault`。
- 板内切换（board↔photo、跳过缺图）都不换 `/live.avi`、不重发 AVI 头、
  不触发 DLNA 重推；只有「发到一半读失败、连接被迫关闭」这一种会走既有
  的 supervisor 重推。

## 6. 对外接口（photo_play.h）

```c
void photo_play_start(void);        /* app_main 在 stream_start 之后调 */
void photo_play_notify_playlist(void); /* photo_sync 每次应用清单后调 */
void photo_play_mark_bad(const uint8_t sha[32], const char *why);
bool photo_is_playing(void);        /* 序列当前项是照片且有效 */
int  photo_play_holds(void);        /* 正在发送的照片引用数（GC/格式化用） */
```

- 调度任务：100ms tick，栈 4096，优先级 2；只改共享快照，不做 IO。
- `photo_is_playing()`：当前序列项是照片且文件有效时 true；供 supervisor
  打日志/LCD 用（推流统计仍以 `stream_clients()` 为准）。
- supervisor：照片播放仍走同一个 `/live.avi`，`streaming` 判据不变，不做
  额外让位；只在 LCD/日志里显示「照片播放中」。

## 7. 验收 / 测试计划

主机（新增 `photo_seq_test`，纯逻辑，注入假时钟）：

- 序列构建：board + 照片按 order；离线索引构建；`duration_s==0` 默认 8s；
  缺文件/bad/超限整条跳过；清单更新后保持当前 sha。
- 时间推进：到点切换、跳过不可播项、board 默认 20s、时长边界误差 ≤1 帧。
- `photo_is_playing()` 的状态。

上板（USB-TV）：

- 无卡：看板行为与 v1.0.0 逐帧一致；
- 有卡：board 20s → 照片按顺序，每张按 duration_s；照片之间无缝；
- 播放中 rm/add：序列重建、不从头乱跳；
- 播放中拔卡：重连后回看板，status=fault；
- 播放中同时触发取帧通知和照片下载：取帧不被照片播放挡，下载不打断照片；
- 1fps / 0.5fps 电视稳定性；堆/任务栈水位。

## 8. 需要 USB-TV 确认的点

1. board 停留用清单里的 `duration_s`（默认 20s）——确认。
2. 开机从 board 开始播，而不是上次停下的照片——建议 board 优先，确认。
3. 清单更新时当前照片仍有效则保持，否则回 board——确认。
4. 照片发到一半读失败时**关闭连接**（不补短块）——确认可接受。
5. `photo_is_playing()` 只在「当前项是照片且已挂卡」时为 true——确认。
6. 2fps 先上线；电视测完 1/0.5fps 再定常量，确认试验顺序。

## 9. 堆预算与维护窗口（2026-10-18 第二轮板测后）

M3 第一版在板子上出现过历史最低 584B。原因不是单个大对象，而是四件事叠加：

| 常驻/瞬态 | 量级 | 说明 |
|---|---|---|
| 时钟带双缓冲 | 26.6KB | v1.0.0 已有，不动 |
| SD 驱动 + 4KB DMA | ~6.7KB | 挂卡时；无缓存照片时开机后立即卸载 |
| 播放清单 `s_pl` | 8.1KB | 在线/离线共用这一份 |
| HTTP 读缓冲 | 2.0KB | 原 4KB，压到 2KB |
| 播放计划/状态/holds/bad | ~1.5KB | 只存下标，不再复制 8.8KB 序列 |
| 失败重试数组 | 2.4KB | 200 项退避 |
| 播放调度任务栈 | 4KB | 常驻 |
| 照片读缓冲 | 4KB/连接 | 只在照片帧；回 board/暂停立刻 free |
| 照片任务栈 | 8KB | 同步期间；已与 TLS 串行 |
| TLS（清单/下载/取帧） | 动态 | 日志记录开始/结束时堆 |

第一版叠加路径：照片播放（4-8KB 缓冲 + SD）× 下载 TLS × 流发送缓冲，
起点只有 55KB 左右，TLS 握手把最低点打到 584B；60KB 清单门槛也经常够不着。

修法：

1. **维护窗口**：清单/下载前 `photo_play_pause()` 切回 board、等正在发的
   照片 hold 归零（冻结当前项计时），网络工作结束后 resume。
2. **stream quiet**：TLS 握手前后 `stream_quiet(true/false)`，停发新帧 1 秒
   排空 WiFi 发送缓冲；期间 `stream_last_active_us` 继续走，supervisor 不误判。
3. **堆门槛 90KB**：取帧/清单/下载在拿 net 锁前等堆回到 90KB（最多 10s）；
   MQTT 未连接时不开 HTTPS，避免两次 TLS 同时握手。
4. **静态腾挪**：删掉重复的 8.8KB 序列、HTTP 缓冲 4K→2K、照片读缓冲
   8K→4K 且回 board 即释放。

上板验收：10 分钟浸泡、期间 add/rm，历史最低 ≥40KB，清单更新 ≤2 分钟。
日志新增 `清单 TLS 开始/结束 堆`、`下载 TLS 开始/结束 堆`、
`照片播放暂停/恢复`，用于核对门槛与最低点。

## 10. 10 张照片场景（第三轮板测后）

现象：稳态 65–80KB，90KB 门槛在停流之前判断，永远不够；下载一次都没开始。
根因：流在发 board 帧时 WiFi/TCP 发送缓冲占着 20–30KB，心跳采样正好落在
发帧期间；门槛放在 `stream_quiet` 之前，等于拿「发帧中的堆」去比 TLS 门槛。

修法：

- **门槛移到 quiet 排空之后**：先 `stream_quiet(true)`、等 1 秒让发送缓冲
  释放，再等堆回到 90KB；此时日志里的堆约 105–108KB。清单、下载、取帧
  统一按这个顺序。
- **一次维护窗口只下一张照片**：下完立刻 resume 播放，1 秒后为下一张开
  新窗口。避免整批下载让电视长时间停帧，也让每次 TLS 都从排空后的堆开始。
- **重建只跟真实变化**：清单 200、下载成功、GC 真删/改、格式化、开机挂卡
  才 bump 代数；304/网络失败/没下载的轮次不再触发“序列重建”。
- **FAULT 卡恢复**：`photo_sd_mount()` 在 FAULT 时 deinit 再 init（限速 5s），
  拔卡再插回后下一次清单/下载重试就能恢复，不必重启。
- **对账日志**：心跳带 `largest`；清单/下载 TLS 开始/结束打 free+largest；
  photo 任务退出打 free/largest/min；首次同步后打一次静态对账
  （`s_pl/s_buf/retry/parser` 的 sizeof）。据此判断稳态是被发送缓冲占着、
  还是任务/HTTP 有泄漏或碎片。

验收仍是 10 张图 10 分钟下完、历史最低 ≥40KB、稳态 ≥100KB。若稳态仍
不足，日志的 largest/任务退出值会指出下一步：TCP 发送缓冲 23K→16K、
LVGL 内存池 16K→12K，或继续压缩播放静态。

## 11. 开机最低点 16.9KB（第四轮板测后）

日志：开机后照片任务立即挂卡/拉清单/下载，与 MQTT TLS、首帧 TLS、LCD/时钟
初始化叠在一起。15:29:49 的 `photo 任务退出` 记录 min=16928。稳态之后
不再下降，说明不是泄漏，是**启动期两次/两次以上 TLS 与 TCP 窗口叠加**。

两个修法：

1. **照片开机任务延后**：`photo_sync_boot_mount()` 不再立即执行，而是起一个
   3072 栈的小任务：等 `cloud_connected()` + `framefetch_first_done()`，
   再等 30 秒，然后才挂卡/拉清单。最长等 90 秒 + 30 秒；离线也能在 2 分钟
   后从索引播缓存照片。日志打“启动条件”“延迟结束”的 free/largest/min。
2. **TCP 窗口从 23K/11.5K 降到 12K/8K**（sdkconfig.defaults）：照片下载时
   同时存在电视拉流和 HTTPS 两个 socket，旧窗口两边合计近 70KB，是 TLS
   握手最低点的主要来源。12K/8K 对 2fps×180KB 帧仍有余量（Wi-Fi RTT 个位
   数毫秒），换回约 30KB 堆。
3. 失败重试数组从 64 位微秒压到 32 位秒：2.4KB→1KB。

新增日志：`首帧取帧结束：free/largest/min`、`启动条件…`、`延迟结束…`、
`photo 任务退出：free/largest/min`，心跳 `largest`。冷启动 + 10 张同步的
验收仍是心跳 min ≥40K、稳态 ≥100K；若稳态仍差一点，日志里的 largest 与
任务退出值用于判断是发送缓冲还是常驻占用。
