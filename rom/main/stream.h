#pragma once
#include <stddef.h>
#include <stdint.h>

#include "jband.h"

#define STREAM_PORT 8080
/* T7 的配对码条带只有 6 位数字；12KB 足够，两块缓冲共 24KB，
 * 给 MQTT TLS 留出余量。T4 做整页看板时再按需调大。 */
#define BAND_CAP (12 * 1024)

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

/* 带的双缓冲：取一块空闲缓冲写，写完发布。发布后下一帧起所有连接都用它 */
uint8_t *stream_band_begin(void);
void stream_band_publish(uint8_t *buf, size_t len, int page);

int stream_clients(void);
uint32_t stream_frames_sent(void);
int64_t stream_last_active_us(void);
