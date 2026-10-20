/*
 * 配对页二维码端到端产物：走设备上同一条 pairband_show -> jb_encode_rows，
 * 把带按推流的三段拼接方式拼回底图，写成完整 JPEG，并打印带长。
 * 之后由 qrdecode.py 用独立解码器（zxing-cpp）从这张 JPEG 里解出 URL，
 * 验证的是 JPEG 量化之后的二维码，不是理想位图。
 *
 * BAND_CAP 装不下也要能量出真实字节数（可行性测量），所以这里的缓冲
 * 比设备的大，带长超 BAND_CAP 只打印警告，不当失败。
 *
 * 用法：qrdump <blank.jpg> <码> <url> <out.jpg>
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "jband.h"
#include "pairband.h"
#include "statusmask.h"
#include "stream.h"

#define BIG_CAP (64 * 1024)

static jb_page_t s_page;
static uint8_t s_band[BIG_CAP];
static size_t s_len;
static int s_row0, s_rows;

uint8_t *stream_band_begin(void) { return s_band; }
size_t stream_band_cap(void) { return sizeof s_band; }
bool stream_band_publish_span(uint8_t *buf, size_t len, int page, int row0, int rows)
{
    (void)buf; (void)page;
    s_row0 = row0; s_rows = rows;
    s_len = len;
    return true;
}
const jb_page_t *stream_page(int idx) { (void)idx; return &s_page; }

int main(int argc, char **argv)
{
    if (argc != 5) { fprintf(stderr, "用法：%s <blank.jpg> <码> <url> <out.jpg>\n", argv[0]); return 2; }
    FILE *f = fopen(argv[1], "rb");
    if (!f) return 2;
    fseek(f, 0, SEEK_END); long l = ftell(f); fseek(f, 0, SEEK_SET);
    unsigned char *blank = malloc((size_t)l);
    if (fread(blank, 1, (size_t)l, f) != (size_t)l) return 2;
    fclose(f);
    if (!jb_parse(&s_page, blank, (size_t)l)) { fprintf(stderr, "底图不可用\n"); return 1; }

    pairband_show(argv[2], argv[3]);
    if (!s_len) { fprintf(stderr, "编码失败\n"); return 1; }

    size_t a = s_page.row_off[s_row0], b = s_page.row_off[s_row0 + s_rows];
    FILE *o = fopen(argv[4], "wb");
    if (!o) return 2;
    fwrite(blank, 1, a, o);
    fwrite(s_band, 1, s_len, o);
    fwrite(blank + b, 1, (size_t)l - b, o);
    fclose(o);
    printf("%s %zu\n", argv[2], s_len);
    return 0;
}
