#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "jband.h"

struct fp_index;

#define STREAM_PORT 8080

/* 帧率的**唯一旋钮**。AVI 头的 dwMicroSecPerFrame / strh.rate 都从它派生
 * （见 stream.c 的 build_header）。2fps 是当前值；1fps 已在三星 Room C 上
 * 连续 3 分钟验证通过，改这里即可。0.5fps 不能只改数字：s_fps 是整数，
 * 需要把 AVI 头写成 rate=1/scale=2，不在 M3 范围。 */
#define STREAM_FPS CONFIG_STICKBOX_DLNA_FPS

/* 时钟条带的单块缓冲上限，双缓冲共 26KB。
 *
 * 必须与 server/go/internal/frame/frame.go 的 frame.BandCap 保持一致。
 *
 * 16KB 的来历（docs/FRAME.md 第 4 节）：时钟带逐行只替换 4 个卡片
 * interval，12 行合计最坏 13,089 字节，出现在 08:28——最宽的数字 8 单个
 * 就比最窄的 1 多 1KB，四个槽凑齐宽数字才顶得到。保守上界 14,626 字节。
 *
 * **不要按整条时钟带算。** 整带是 28,885~32,315 字节，双缓冲就是 63KB，
 * 而 T8 实测带 MQTT TLS 的历史最低空闲堆正好是 63.5KB——那条路会把余量
 * 吃光，设备必然 OOM。
 *
 * 光靠这个常量也不够：段长会随渲染器的字体和 q 值变化。帧包自带
 * max_band_bytes，设备加载时先拿 stream_band_cap() 比对，超了拒绝整包。 */
/* 17KB（#31）：配对页贴二维码之后，最坏配对条带（888888 + 任意设备 ID 的 v3 二维码）
 * 实测 16,355..16,560 字节（3000 个随机设备 ID），再留约 850 字节。
 * 原来 13KB：服务端实测最坏时钟带 13,089（08:28，max_band_bytes），时钟带照旧
 * 装得下。两块缓冲各涨 4KB，常驻堆多占 8KB——代价和数据见 #31 的 PR。 */
#define BAND_CAP (17 * 1024)

/* 预置页面：1080p baseline JPEG，DRI = 每行 MCU 数（tools/render_pages.py 生成） */
typedef struct {
    const uint8_t *data;
    size_t len;
} frame_t;

/* band_row0 / band_rows：每帧把这几行换成最新发布的「带」 */
void stream_start(const frame_t *frames, int n, int fps, int band_row0, int band_rows);

const jb_page_t *stream_page(int idx);
int stream_frame_count(void);
int stream_current(void);
void stream_set(int idx);

/* 带的双缓冲：取一块空闲缓冲写，写完发布。发布后下一帧起所有连接都用它。
 *
 * publish 会校验 len：超过 BAND_CAP 就拒绝发布并返回 false，画面停在上一
 * 版而不是把缓冲区外的垃圾推给电视。写入方自己也必须守住上限——越界在
 * 写的时候就已经发生了，这里只是最后一道能被看见的关卡。 */
uint8_t *stream_band_begin(void);
bool stream_band_publish(uint8_t *buf, size_t len, int page);
/* 同上，但这条带替换的是 [row0, row0+rows) 行，而不是 stream_start 给的默认区间。
 * 配对页要把二维码画在默认区间上方，用它发布更高的一条带；状态页仍用默认区间。
 * 行区间超出页面返回 false。 */
bool stream_band_publish_span(uint8_t *buf, size_t len, int page, int row0, int rows);

/* 单块带缓冲的容量。加载帧包时拿它和 meta 里的 max_band_bytes 比对。 */
size_t stream_band_cap(void);

/* ---- 帧包模式（T13b）----
 *
 * 切进来之后推流不再发预置页，改发「底帧 + 12 个行段」的段序列。底帧那些
 * 段直接指向 flash 映射地址，整帧从不在 RAM 里拼起来。
 *
 * base 必须在整个帧包生效期间保持有效（framepack 的 mmap 地址）。 */
void stream_use_framepack(const struct fp_index *ix, const uint8_t *base, size_t base_len);

/* 退回预置页。没有帧包、或帧包失效时调用——画面停在配对页，不黑屏。 */
void stream_use_presets(void);

/* 同上，但不等在途帧发完。只给“马上重启”的清理路径用；不 munmap。 */
void stream_use_presets_nowait(void);

/* 发布一块帧包模式的时钟带，附带每行在缓冲里的偏移与长度。
 * 长度超过 BAND_CAP 时拒绝发布并返回 false，画面保持上一帧。 */
bool stream_band_publish_rows(uint8_t *buf, size_t len,
                              const uint32_t row_off[12], const uint32_t row_len[12]);

int stream_clients(void);
uint32_t stream_frames_sent(void);
int64_t stream_last_active_us(void);

/* 网络维护窗口：暂停发帧，让 WiFi 发送缓冲释放，给 TLS 握手留出堆。
 * 只由持有 photo_io net 锁的一方调用（同一时刻只有一个 owner）；
 * 期间保持 stream_last_active_us 前进，supervisor 不会误判断流重推。 */
void stream_quiet(bool on);
