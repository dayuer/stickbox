/*
 * 状态文字条带：在纯色底图上显示「正在连接…」这类功能性文字。
 *
 * 为什么必须有它：删掉 demo 图之后，设备在「已配对但还没拿到帧包」这个
 * 状态下会给电视推一张纯色底 + 一条空白带——用户看到的就是一条什么都
 * 没有的横条，而设备和服务端都不报错。这个情况实际发生过一次。
 *
 * 文字是构建期预渲染好的 1-bit 掩码（tools/gen_statusmask.py），这里只
 * 查位，不做排版也不碰字体。
 */
#include "statusband.h"

#include <string.h>

#include "esp_log.h"
#include "pairband.h"
#include "statusmask.h"
#include "stream.h"

static const char *TAG = "statusband";

/* 背景色必须和底图一致，否则这条带在画面上就是一道突兀的色条。
 *
 * 这里踩过一次：pairband 的 BG 是 0x1E3A8A，那是**旧 page1 的蓝色背景**。
 * 换成纯色底图（#1D2D3D）之后没改它，电视上就多了一条蓝带——当时报上来
 * 的现象正是「只剩一条蓝色光带」。 */
#define BG 0x1D2D3D
#define FG 0xF5F5F8   /* 设计稿的前景白 */

/* 条带复用配对码那几行（MCU 行 46..51，y 736..831，共 96 像素）。 */
#define BAND_Y0 (PAIR_ROW0 * 16)
#define BAND_H  (PAIR_ROWS * 16)

static const status_mask_t *s_mask;
static int s_x0, s_y0;

static bool shade(int x, int y, void *ctx)
{
    (void)ctx;
    const status_mask_t *m = s_mask;
    if (!m) return false;
    int mx = x - s_x0, my = y - s_y0;
    if (mx < 0 || my < 0 || mx >= m->w || my >= m->h) return false;
    return (m->bits[my * m->stride + (mx >> 3)] >> (7 - (mx & 7))) & 1;
}

bool statusband_show(status_id_t id)
{
    if (id < 0 || id >= STATUS_COUNT) return false;
    const status_mask_t *m = &STATUS_MASKS[id];

    /* 水平居中；垂直在条带内居中。 */
    s_mask = m;
    s_x0 = (1920 - m->w) / 2;
    s_y0 = BAND_Y0 + (BAND_H - m->h) / 2;

    const jb_box_t box = { s_x0, s_y0, s_x0 + m->w, s_y0 + m->h };
    uint8_t *buf = stream_band_begin();
    size_t n = jb_encode_rows(stream_page(0), PAIR_ROW0, PAIR_ROW0 + PAIR_ROWS,
                              BG, FG, shade, NULL, &box, buf, stream_band_cap());
    if (!n) {
        ESP_LOGE(TAG, "状态文字编码超出缓冲 %u", (unsigned)stream_band_cap());
        return false;
    }
    if (!stream_band_publish(buf, n, 0)) return false;
    ESP_LOGI(TAG, "状态页：%d（条带 %u 字节）", (int)id, (unsigned)n);
    return true;
}
