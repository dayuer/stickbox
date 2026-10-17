#pragma once
/*
 * 「局部重编码」JPEG：预置页面每个 MCU 行是一个 restart 段（DRI = 每行 MCU 数），
 * 段与段之间 DC 预测清零，所以任意几行可以单独重新编码、再原样拼回去。
 *
 * 一帧 = 页面 [0, row_off[r0])  +  板上编码的 r0..r1-1 行  +  页面 [row_off[r1], len)
 *
 * 只支持本工程自己生成的图：baseline、8 位、YCbCr 4:2:0、两张量化表、标准 4 张哈夫曼表。
 */
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#define JB_MAX_ROWS 68

typedef struct {
    const uint8_t *jpg;
    size_t len;
    int w, h, mcu_cols, mcu_rows;
    uint32_t row_off[JB_MAX_ROWS + 1];  /* 第 r 行熵编码数据起点；[mcu_rows] = EOI 位置 */
} jb_page_t;

/* 解析页面；第一张页面的量化 / 哈夫曼表被当作编码器的表（所有页面必须同表） */
bool jb_parse(jb_page_t *p, const uint8_t *jpg, size_t len);

/* 像素着色：返回 true = 前景色。x/y 是整图坐标 */
typedef bool (*jb_shader_t)(int x, int y, void *ctx);

/* 编码 [r0, r1) 行，背景 bg / 前景 fg（RGB888），结果写进 out；
 * 每行末尾补对应的 RSTn（最后一行如果是整图最后一行则不补）。返回字节数，0 = 缓冲不够 */
typedef struct { int x0, y0, x1, y1; } jb_box_t;  /* 前景只可能出现在这个框里 */

size_t jb_encode_rows(const jb_page_t *p, int r0, int r1, uint32_t bg, uint32_t fg,
                      jb_shader_t shade, void *ctx, const jb_box_t *box, uint8_t *out, size_t cap);
