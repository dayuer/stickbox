/*
 * 配对码条带：七段数码管，纯矩形，不用字体。
 * 版面按 1080p / MCU 行 46..51（y 736..831）设计，6 位数字居中。
 */
#include "pairband.h"

#include <string.h>

#include "esp_log.h"
#include "stream.h"

static const char *TAG = "pairband";

/* 56px 高、36px 宽：1080p 上够看清，且最坏码 888888 编码约 10.3KB，
 * 在 12KB 缓冲内。80px 的旧版面 888888 会超 12KB，随机码里迟早会撞上。 */
#define X0   822
#define Y0   752
#define W     36
#define H     56
#define T      7
#define GAP   12
#define NDIG   6
/* 必须和底图背景一致。原来是 0x1E3A8A（旧 page1 的蓝色背景），换成纯色
 * 底图之后忘了改，电视上就多了一道蓝带——这个现象实际报上来过。 */
#define BG   0x1D2D3D
#define FG   0xFFFFFF

const jb_box_t PAIRBAND_BOX = {
    X0, Y0, X0 + NDIG * W + (NDIG - 1) * GAP, Y0 + H
};

/*  段位：A 上 B 右上 C 右下 D 下 E 左下 F 左上 G 中 */
enum { A = 1, B = 2, C = 4, D = 8, E = 16, F = 32, G = 64 };
static const unsigned char SEG[10] = {
    A | B | C | D | E | F, B | C, A | B | D | E | G, A | B | C | D | G, B | C | F | G,
    A | C | D | F | G, A | C | D | E | F | G, A | B | C, 0x7F, A | B | C | D | F | G,
};

static bool in(int x, int y, int x0, int y0, int x1, int y1)
{
    return x >= x0 && x < x1 && y >= y0 && y < y1;
}

static bool digit(unsigned char s, int x, int y)
{
    const int m0 = H / 2 - T / 2, m1 = H / 2 + T / 2;
    return ((s & A) && in(x, y, T, 0, W - T, T)) ||
           ((s & B) && in(x, y, W - T, T, W, m0)) ||
           ((s & C) && in(x, y, W - T, m1, W, H - T)) ||
           ((s & D) && in(x, y, T, H - T, W - T, H)) ||
           ((s & E) && in(x, y, 0, m1, T, H - T)) ||
           ((s & F) && in(x, y, 0, T, T, m0)) ||
           ((s & G) && in(x, y, T, m0, W - T, m1));
}

static bool pair_shade(int x, int y, void *ctx)
{
    if (x < X0 || y < Y0 || y >= Y0 + H) return false;
    const char *code = ctx;
    int i = (x - X0) / (W + GAP);
    int lx = (x - X0) - i * (W + GAP);
    if (i < 0 || i >= NDIG || lx >= W) return false;
    char ch = code[i];
    if (ch < '0' || ch > '9') return false;
    return digit(SEG[ch - '0'], lx, y - Y0);
}

static size_t encode(const char *code)
{
    uint8_t *buf = stream_band_begin();
    size_t n = jb_encode_rows(stream_page(0), PAIR_ROW0, PAIR_ROW0 + PAIR_ROWS,
                              BG, FG, pair_shade, (void *)code,
                              &PAIRBAND_BOX, buf, BAND_CAP);
    if (!n) {
        ESP_LOGE(TAG, "配对码条带超出缓冲 %d B", BAND_CAP);
        return 0;
    }
    stream_band_publish(buf, n, 0);
    return n;
}

void pairband_show(const char *code)
{
    if (!code || strlen(code) != NDIG) return;
    size_t n = encode(code);
    if (n) ESP_LOGI(TAG, "配对码 %s 已上电视，条带 %u B", code, (unsigned)n);
}

void pairband_clear(void)
{
    /* box 为空，shade 永远不会被调用；整条带编码成背景色。 */
    static const jb_box_t empty = { 0, 0, 0, 0 };
    uint8_t *buf = stream_band_begin();
    size_t n = jb_encode_rows(stream_page(0), PAIR_ROW0, PAIR_ROW0 + PAIR_ROWS,
                              BG, BG, NULL, NULL, &empty, buf, BAND_CAP);
    if (!n) {
        ESP_LOGE(TAG, "清配对码条带失败（缓冲不够）");
        return;
    }
    stream_band_publish(buf, n, 0);
}
