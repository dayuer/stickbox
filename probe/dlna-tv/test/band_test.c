/* 主机侧校验：把 jband.c / clockface.c 原样编到 Mac 上，拼一帧带时钟的 JPEG 出来，
 * 再交给 PIL / ffmpeg 解码比对。用法见 test/run.sh。 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "clockface.h"
#include "jband.h"

static uint8_t *slurp(const char *p, size_t *n)
{
    FILE *f = fopen(p, "rb");
    if (!f) return NULL;
    fseek(f, 0, SEEK_END);
    *n = ftell(f);
    fseek(f, 0, SEEK_SET);
    uint8_t *b = malloc(*n);
    fread(b, 1, *n, f);
    fclose(f);
    return b;
}

int main(int argc, char **argv)
{
    if (argc < 6) { fprintf(stderr, "用法: page.jpg bg_hex HH:MM:SS out.jpg loops\n"); return 2; }
    size_t n;
    uint8_t *jpg = slurp(argv[1], &n);
    jb_page_t p;
    if (!jpg || !jb_parse(&p, jpg, n)) { fprintf(stderr, "parse 失败\n"); return 1; }
    printf("%dx%d mcu %dx%d  rows[%d..%d) 原长 %u B\n", p.w, p.h, p.mcu_cols, p.mcu_rows,
           CLOCK_ROW0, CLOCK_ROW0 + CLOCK_ROWS,
           (unsigned)(p.row_off[CLOCK_ROW0 + CLOCK_ROWS] - p.row_off[CLOCK_ROW0]));

    clockface_t c;
    strncpy(c.txt, argv[3], sizeof c.txt);
    c.colon_on = true;
    static uint8_t band[64 * 1024];
    uint32_t bg = strtoul(argv[2], NULL, 16);
    int loops = atoi(argv[5]);
    clock_t t0 = clock();
    size_t bn = 0;
    for (int i = 0; i < loops; i++)
        bn = jb_encode_rows(&p, CLOCK_ROW0, CLOCK_ROW0 + CLOCK_ROWS, bg, 0xFFFFFF, clockface_shade, &c,
                            &CLOCKFACE_BOX, band, sizeof band);
    printf("带编码 %zu B，%.2f ms/次\n", bn, 1000.0 * (clock() - t0) / CLOCKS_PER_SEC / loops);
    if (!bn) return 1;

    FILE *o = fopen(argv[4], "wb");
    fwrite(jpg, 1, p.row_off[CLOCK_ROW0], o);
    fwrite(band, 1, bn, o);
    fwrite(jpg + p.row_off[CLOCK_ROW0 + CLOCK_ROWS], 1, n - p.row_off[CLOCK_ROW0 + CLOCK_ROWS], o);
    fclose(o);
    return 0;
}
