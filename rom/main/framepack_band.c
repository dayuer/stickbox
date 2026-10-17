/*
 * 帧包拼接核心（T13b）。
 *
 * **这个文件不依赖 ESP-IDF**，可以在主机上编译。rom/tools/bandtest 用
 * T13a 产出的黄金帧包跑它，再和 T13a 独立拼出来的 spliced.jpg 逐字节比对
 * ——两个独立实现对上同一串字节，才算这套字节规则两边理解一致。
 *
 * 规则见 docs/FRAME.md 第 4.2 节。核心只有三句话：
 *   - 卡片宽 8 个 MCU，DRI=8，所以一个卡片行段恰好是一个 restart 区间，
 *     DC 在区间起点清零、区间内自足；
 *   - 于是同一个数字段放到任何槽位，熵编码字节完全通用，只有末尾那个
 *     RSTn 的序号跟位置走；
 *   - 替换段和被替换段的区间数相同，后面所有 MCU 的 RSTn 序号不变。
 * 这三条合起来，设备每分钟要做的就只是 memcpy 加改 48 个字节。
 */
#include <string.h>

#include "framepack.h"

/* 这一行在整帧里的 restart 区间序号。
 * 绝对 MCU 序号 = 绝对行 × 每行 MCU 数，而每行 MCU 数能被 DRI 整除，
 * 所以区间序号可以直接按行累加，不必展开到 MCU。 */
static inline uint32_t interval_index(const fp_index_t *ix, int row, int slot)
{
    return (uint32_t)(ix->band_row0 + row) * ix->intervals_per_row
         + ix->slot_mcu_col[slot] / 8;
}

/* HH:MM 拆成四个槽的数字：时十位、时个位、分十位、分个位。 */
static bool split_digits(int hh, int mm, int d[FP_SLOTS])
{
    if (hh < 0 || hh > 23 || mm < 0 || mm > 59) return false;
    d[0] = hh / 10; d[1] = hh % 10;
    d[2] = mm / 10; d[3] = mm % 10;
    return true;
}

void fp_band_layout(const fp_index_t *ix, uint32_t out_off[FP_BAND_ROWS],
                    uint32_t out_len[FP_BAND_ROWS], int hh, int mm)
{
    int d[FP_SLOTS];
    uint32_t cursor = 0;
    bool ok = split_digits(hh, mm, d);

    for (int row = 0; row < FP_BAND_ROWS; row++) {
        uint32_t len = ix->row_runs[row].len;
        if (ok) {
            /* 行长 = 原行长 - 四个被换掉的段 + 四个换上去的段。 */
            for (int s = 0; s < FP_SLOTS; s++)
                len = len - ix->slot_runs[s][row].len + ix->digit_runs[d[s]][row].len;
        }
        out_off[row] = cursor;
        out_len[row] = len;
        cursor += len;
    }
}

size_t fp_build_band(const fp_index_t *ix,
                     const uint8_t *base, size_t base_len,
                     const uint8_t *atlas, size_t atlas_len,
                     int hh, int mm,
                     uint8_t *out, size_t cap)
{
    int d[FP_SLOTS];
    if (!ix || !base || !atlas || !out) return 0;
    if (!split_digits(hh, mm, d)) return 0;

    size_t w = 0;

    for (int row = 0; row < FP_BAND_ROWS; row++) {
        const fp_run_t *rr = &ix->row_runs[row];
        if ((size_t)rr->off + rr->len > base_len) return 0;

        /* 四个槽在本行里按 off 升序，而且恰好都落在 row_run 内——这是
         * 服务端的保证，但别信它，越界会把别处的内存拼进画面。 */
        uint32_t cur = 0;    /* 行内游标，相对 rr->off */

        for (int s = 0; s < FP_SLOTS; s++) {
            const fp_run_t *sr = &ix->slot_runs[s][row];
            const fp_run_t *dr = &ix->digit_runs[d[s]][row];

            if (sr->off < rr->off) return 0;
            uint32_t rel = sr->off - rr->off;
            if ((uint64_t)rel + sr->len > rr->len) return 0;   /* 槽不在行内 */
            if (rel < cur) return 0;                            /* 槽没有升序 */
            if ((size_t)dr->off + dr->len > atlas_len) return 0;
            if (dr->len == 0) return 0;

            /* 槽之前的原始字节（含上一个槽到这个槽之间的冒号等内容）。 */
            uint32_t gap = rel - cur;
            if (w + gap + dr->len > cap) return 0;
            memcpy(out + w, base + rr->off + cur, gap);
            w += gap;

            /* 数字段，然后把末字节改成本位置应有的 RSTn 序号。
             * 段固定以 2 字节 FF D0+n 收尾，改末字节不碰字节填充。 */
            memcpy(out + w, atlas + dr->off, dr->len);
            out[w + dr->len - 1] = (uint8_t)(0xD0 + (interval_index(ix, row, s) % 8));
            w += dr->len;

            cur = rel + sr->len;
        }

        /* 本行最后一个槽之后的剩余字节。 */
        uint32_t tail = rr->len - cur;
        if (w + tail > cap) return 0;
        memcpy(out + w, base + rr->off + cur, tail);
        w += tail;
    }
    return w;
}
