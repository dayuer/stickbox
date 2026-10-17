/*
 * meta.json → fp_index_t（T13b）。
 *
 * **不依赖 ESP-IDF**，和 framepack_band.c 一样能在主机上编译，所以
 * rom/tools/bandtest 验的就是设备上真正跑的这份解析器，不是另写一份。
 *
 * 为什么不用 cJSON：meta 约 5KB、含 180 个 run 共 360 个数字，cJSON 会给
 * 每个节点分配一次，瞬时几十 KB 的堆。设备稳态空闲约 140KB，还要同时扛
 * MQTT TLS 和推流——这笔开销不值得。这里是单遍扫描、零分配。
 *
 * 输入来自网络，一律当不可信处理：长度、范围、顺序都要查，查不过就整包拒绝。
 */
#include <stdlib.h>
#include <string.h>

#include "framepack.h"

/* 在 [p, end) 里找 "key"，返回键名之后的位置；找不到返回 NULL。 */
static const char *find_key(const char *p, const char *end, const char *key)
{
    size_t klen = strlen(key);
    for (; p + klen + 2 <= end; p++) {
        if (*p == '"' && (size_t)(end - p) > klen + 1 &&
            memcmp(p + 1, key, klen) == 0 && p[1 + klen] == '"')
            return p + klen + 2;
    }
    return NULL;
}

/* 读下一个十进制整数。失败返回 false，不移动游标。 */
static bool next_num(const char **p, const char *end, long *out)
{
    const char *s = *p;
    while (s < end && (*s < '0' || *s > '9') && *s != '-') s++;
    if (s >= end) return false;
    char *stop = NULL;
    long v = strtol(s, &stop, 10);
    if (!stop || stop == s) return false;
    *p = stop;
    *out = v;
    return true;
}

/* 读 "key": <整数> */
static bool key_num(const char *p, const char *end, const char *key, long *out)
{
    const char *k = find_key(p, end, key);
    if (!k) return false;
    return next_num(&k, end, out);
}

/* 从游标处读 count 个 {"off":N,"len":M}。读完后游标停在最后一段之后。 */
static bool read_runs(const char **p, const char *end, fp_run_t *out, int count)
{
    for (int i = 0; i < count; i++) {
        const char *o = find_key(*p, end, "off");
        if (!o) return false;
        long off, len;
        if (!next_num(&o, end, &off)) return false;
        const char *l = find_key(o, end, "len");
        if (!l) return false;
        if (!next_num(&l, end, &len)) return false;
        if (off < 0 || len <= 0) return false;
        out[i].off = (uint32_t)off;
        out[i].len = (uint32_t)len;
        *p = l;
    }
    return true;
}

bool fp_parse_meta(const char *json, size_t len, fp_index_t *ix)
{
    if (!json || !ix || len == 0) return false;
    const char *end = json + len;
    memset(ix, 0, sizeof *ix);

    long dri = 8, cols = 120;
    key_num(json, end, "dri", &dri);
    key_num(json, end, "mcu_cols", &cols);
    if (dri <= 0 || cols <= 0 || cols % dri != 0) return false;
    ix->intervals_per_row = (uint16_t)(cols / dri);

    /* "rows":[4,16] —— 第一个数是时钟带的起始 MCU 行。 */
    long row0 = 0;
    if (!key_num(json, end, "rows", &row0) || row0 < 0 || row0 > 4096) return false;
    ix->band_row0 = (uint16_t)row0;

    long mbb = 0;
    if (!key_num(json, end, "max_band_bytes", &mbb) || mbb <= 0) return false;
    ix->max_band_bytes = (uint32_t)mbb;

    const char *p = find_key(json, end, "row_runs");
    if (!p || !read_runs(&p, end, ix->row_runs, FP_BAND_ROWS)) return false;

    p = find_key(json, end, "slots");
    if (!p) return false;
    for (int s = 0; s < FP_SLOTS; s++) {
        const char *mc = find_key(p, end, "mcu_col");
        if (!mc) return false;
        long col;
        if (!next_num(&mc, end, &col) || col < 0 || col > cols) return false;
        /* 卡片左边界必须落在 restart 边界上，否则「一个卡片行段 = 一个
         * restart 区间」这个前提不成立，拼出来的是花屏。 */
        if (col % dri != 0) return false;
        ix->slot_mcu_col[s] = (uint16_t)col;
        p = mc;
        if (!read_runs(&p, end, ix->slot_runs[s], FP_BAND_ROWS)) return false;
    }

    p = find_key(json, end, "digits");
    if (!p) return false;
    for (int d = 0; d < FP_DIGITS; d++) {
        /* 顺序即 0–9；服务端就是这么写的，但别信，查一下 "d" 的值。 */
        const char *dk = find_key(p, end, "d");
        if (!dk) return false;
        long dv;
        if (!next_num(&dk, end, &dv) || dv != d) return false;
        p = dk;
        if (!read_runs(&p, end, ix->digit_runs[d], FP_BAND_ROWS)) return false;
    }
    return true;
}

bool fp_index_valid(const fp_index_t *ix, size_t base_len, size_t atlas_len,
                    size_t band_cap, const char **why)
{
    const char *dummy;
    if (!why) why = &dummy;

    if (ix->max_band_bytes > band_cap) {
        /* 段长会随渲染器的字体和 q 值变化，光靠两边的常量对齐不够。
         * 宁可整包不要，也不能拼到一半越界。 */
        *why = "max_band_bytes 超过本机缓冲";
        return false;
    }
    for (int row = 0; row < FP_BAND_ROWS; row++) {
        const fp_run_t *rr = &ix->row_runs[row];
        if ((size_t)rr->off + rr->len > base_len) { *why = "行段超出底帧"; return false; }

        uint32_t cur = 0;
        for (int s = 0; s < FP_SLOTS; s++) {
            const fp_run_t *sr = &ix->slot_runs[s][row];
            if (sr->off < rr->off) { *why = "槽在行首之前"; return false; }
            uint32_t rel = sr->off - rr->off;
            if ((uint64_t)rel + sr->len > rr->len) { *why = "槽超出行段"; return false; }
            if (rel < cur) { *why = "槽未按列升序"; return false; }
            cur = rel + sr->len;

            for (int d = 0; d < FP_DIGITS; d++) {
                const fp_run_t *dr = &ix->digit_runs[d][row];
                if ((size_t)dr->off + dr->len > atlas_len) {
                    *why = "数字段超出图集";
                    return false;
                }
            }
        }
    }
    *why = "";
    return true;
}
