#pragma once
#include <stdbool.h>

/* 时钟带：MCU 行 50..59 = y 800..959。改了要同步 tools/render_pages.py 的 BAND_Y0/Y1 */
#define CLOCK_ROW0 50
#define CLOCK_ROWS 10

typedef struct {
    char txt[9];    /* "HH:MM"；没对上时用 "--:--" */
    bool colon_on;  /* 冒号逐秒闪烁，分钟级显示也能看出在走 */
} clockface_t;

/* jb_shader_t：七段数码管，纯矩形，不用字体 */
bool clockface_shade(int x, int y, void *ctx);

/* 前景外接框（"88:88" 的范围），给 jb_encode_rows 跳过空白 MCU */
#include "jband.h"
extern const jb_box_t CLOCKFACE_BOX;
