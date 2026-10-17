#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

/* 帧包：服务端渲染的底帧 + 数字图集 + 索引（T13b）。
 *
 * 协议与字节规则见 docs/FRAME.md。本头文件分两层：
 *   - 下半部分的拼接核心是**纯 C、不依赖 ESP-IDF**，可以在主机上编译，
 *     拿 T13a 的黄金产物逐字节比对（见 rom/tools/bandtest）。
 *   - 上半部分是设备侧的存取与生命周期，依赖 IDF。
 */

#define FP_SLOTS      4    /* HH:MM 四个数字槽 */
#define FP_BAND_ROWS 12    /* 时钟带占 12 个 MCU 行 */
#define FP_DIGITS    10

/* 一段连续字节。off 的基准见各字段说明。 */
typedef struct {
    uint32_t off;
    uint32_t len;
} fp_run_t;

/* 帧包索引：meta.json 解析后的结果。只留设备拼接真正用得到的字段。 */
typedef struct {
    /* 每个 MCU 行的时钟列区间（MCU 列 72..112），off 相对底帧起点。 */
    fp_run_t row_runs[FP_BAND_ROWS];
    /* 四个卡片槽在底帧里的位置，off 相对底帧起点。 */
    fp_run_t slot_runs[FP_SLOTS][FP_BAND_ROWS];
    /* 每个槽的 MCU 列，用来算 RSTn 序号。 */
    uint16_t slot_mcu_col[FP_SLOTS];
    /* 0–9 各数字的 12 段，off 相对 atlas 起点。 */
    fp_run_t digit_runs[FP_DIGITS][FP_BAND_ROWS];
    /* 时钟带起始 MCU 行（FRAME.md 里是 4）。算 RSTn 要用。 */
    uint16_t band_row0;
    /* 每个 MCU 行有多少个 restart 区间（1920/16/DRI = 15）。 */
    uint16_t intervals_per_row;
    /* 服务端遍历 1440 个时刻算出的最坏拼接长度。加载时必须校验它
     * 不超过 BAND_CAP——段长会随渲染器的字体和 q 值变化，光靠常量不够。 */
    uint32_t max_band_bytes;
} fp_index_t;

/* 按 HH:MM 拼出时钟带。
 *
 * base/atlas 可以是 flash 映射地址，函数只读不写它们。
 * out 至少要有 cap 字节；返回写入的字节数，0 表示失败（cap 不够或索引不合法）。
 *
 * 输出布局是 12 个行段首尾相接，每段对应 row_runs[i]——调用方按
 * row_runs 的绝对偏移把它们拼回推流里。
 *
 * hh/mm 超出范围会被拒绝：拼一张错的画面不如不拼。 */
size_t fp_build_band(const fp_index_t *ix,
                     const uint8_t *base, size_t base_len,
                     const uint8_t *atlas, size_t atlas_len,
                     int hh, int mm,
                     uint8_t *out, size_t cap);

/* 拼接后各行段在输出缓冲里的偏移，和 fp_build_band 的写入顺序一致。
 * 推流时要用它把行段和底帧的连接段交替发出去。 */
void fp_band_layout(const fp_index_t *ix, uint32_t out_off[FP_BAND_ROWS],
                    uint32_t out_len[FP_BAND_ROWS], int hh, int mm);
