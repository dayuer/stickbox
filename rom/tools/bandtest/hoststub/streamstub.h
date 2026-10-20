#pragma once
/*
 * 主机侧 stream 桩：pairband.c 用到的那几个 stream_* 接口，加上对
 * 「配对页大缓冲」（stream_extra_*）的账本——申请/释放次数、是否持锁、
 * 当前是否已分配。测试靠它钉死：
 *   - acquire 之后必须以 publish 或 abort 收尾（不持锁泄漏）；
 *   - release 幂等，配对-清除循环结束后大缓冲必须是未分配；
 *   - acquire 失败（模拟堆不够）时降级为只画文字，仍能装进常驻 BAND_CAP。
 * 只能被一个编译单元 include（全是 static 定义）。
 */
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "jband.h"
#include "pairband.h"
#include "stream.h"

static jb_page_t s_page;
static uint8_t s_band[BAND_CAP];
static uint8_t s_extra_mem[PAIR_BAND_CAP];
static size_t s_len;                 /* 最近一次发布的带长 */
static size_t s_len_cap;             /* 发布到的那块缓冲的容量 */
static int s_row0, s_rows;           /* 最近一次发布的行区间 */
static bool s_pub_extra;             /* 最近一次发布用的是大缓冲 */

static bool x_alloc, x_lock, x_fail; /* 已分配 / 持锁 / 下次 acquire 模拟 malloc 失败 */
static int x_acq, x_rel, x_pub, x_abort, x_double_lock, x_bad_pub;

uint8_t *stream_band_begin(void) { return s_band; }
size_t stream_band_cap(void) { return sizeof s_band; }
const jb_page_t *stream_page(int idx) { (void)idx; return &s_page; }

static bool stub_publish(uint8_t *buf, size_t len, int row0, int rows)
{
    s_len = len; s_row0 = row0; s_rows = rows;
    s_pub_extra = buf == s_extra_mem;
    s_len_cap = s_pub_extra ? sizeof s_extra_mem : sizeof s_band;
    return true;
}

bool stream_band_publish(uint8_t *buf, size_t len, int page)
{
    (void)page;
    return stub_publish(buf, len, PAIR_ROW0, PAIR_ROWS);
}
bool stream_band_publish_span(uint8_t *buf, size_t len, int page, int row0, int rows)
{
    (void)page;
    return stub_publish(buf, len, row0, rows);
}

uint8_t *stream_extra_acquire(void)
{
    if (x_lock) x_double_lock++;
    if (!x_alloc) {
        if (x_fail) return NULL;
        x_alloc = true;
    }
    x_lock = true;
    x_acq++;
    return s_extra_mem;
}
bool stream_extra_publish(uint8_t *buf, size_t len, int page, int row0, int rows)
{
    (void)page;
    if (!x_lock || buf != s_extra_mem) x_bad_pub++;
    x_lock = false;
    x_pub++;
    return stub_publish(buf, len, row0, rows);
}
void stream_extra_abort(void)
{
    if (!x_lock) x_bad_pub++;
    x_lock = false;
    x_abort++;
}
void stream_extra_release(void)
{
    if (x_lock) x_double_lock++;
    x_alloc = false;
    x_rel++;
}
