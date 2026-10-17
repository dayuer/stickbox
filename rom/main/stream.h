#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "jband.h"

struct fp_index;

#define STREAM_PORT 8080

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
/* 13KB：服务端实测最坏带长 13,089（08:28，max_band_bytes），再留 223 字节。
 * 两块缓冲各 16KB 是这个版本稳态比 v0.9.3 少 45KB 的大头；压到 13KB
 * 省下 6KB，且仍然覆盖最坏帧。 */
#define BAND_CAP (13 * 1024)

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

/* 发布一块帧包模式的时钟带，附带每行在缓冲里的偏移与长度。
 * 长度超过 BAND_CAP 时拒绝发布并返回 false，画面保持上一帧。 */
bool stream_band_publish_rows(uint8_t *buf, size_t len,
                              const uint32_t row_off[12], const uint32_t row_len[12]);

int stream_clients(void);
uint32_t stream_frames_sent(void);
int64_t stream_last_active_us(void);
