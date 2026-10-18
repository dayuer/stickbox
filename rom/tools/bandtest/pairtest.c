/*
 * 配对页主机侧验证：6 位数字 + 提示文字编码后必须装进 BAND_CAP。
 *
 * 设备上 BAND_CAP 压到 13KB（两块带缓冲，省 6KB 堆）。配对码最坏是
 * 888888，再加上新加的提示文字；这个测试走与设备完全相同的
 * pairband 编码路径，用真实 blank.jpg 的 MCU 布局算字节数。
 *
 * 用法：pairtest <blank.jpg>
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "jband.h"
#include "pairband.h"
#include "statusmask.h"
#include "stream.h"

static jb_page_t s_page;
static uint8_t s_band[BAND_CAP];
static size_t s_len;

/* pairband.c 需要的最小 stream 桩。 */
uint8_t *stream_band_begin(void) { return s_band; }
size_t stream_band_cap(void) { return sizeof s_band; }
bool stream_band_publish(uint8_t *buf, size_t len, int page)
{
    (void)buf;
    (void)page;
    s_len = len;
    return true;
}
const jb_page_t *stream_page(int idx)
{
    (void)idx;
    return &s_page;
}

static unsigned char *slurp(const char *p, size_t *n)
{
    FILE *f = fopen(p, "rb");
    if (!f) { fprintf(stderr, "打不开 %s\n", p); exit(2); }
    fseek(f, 0, SEEK_END); long l = ftell(f); fseek(f, 0, SEEK_SET);
    unsigned char *b = malloc((size_t)l);
    if (fread(b, 1, (size_t)l, f) != (size_t)l) exit(2);
    fclose(f);
    *n = (size_t)l;
    return b;
}

int main(int argc, char **argv)
{
    if (argc != 2) {
        fprintf(stderr, "用法：%s <blank.jpg>\n", argv[0]);
        return 2;
    }
    size_t blen;
    unsigned char *blank = slurp(argv[1], &blen);
    if (!jb_parse(&s_page, blank, blen)) {
        fprintf(stderr, "底图不是本工程能用的 JPEG\n");
        return 1;
    }

    /* 最坏码：六个 8 的段最多。 */
    pairband_show("888888");
    printf("配对页最坏带长 %zu 字节（BAND_CAP %d）\n", s_len, BAND_CAP);
    if (!s_len || s_len > BAND_CAP) {
        fprintf(stderr, "✗ 配对页超出带缓冲\n");
        return 1;
    }
    if (STATUS_MASKS[STATUS_PAIR_HINT].w > 1000 ||
        STATUS_MASKS[STATUS_PAIR_HINT].h > 40) {
        fprintf(stderr, "✗ 提示文字尺寸异常\n");
        return 1;
    }
    return 0;
}
