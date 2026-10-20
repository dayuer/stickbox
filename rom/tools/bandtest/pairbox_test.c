/*
 * 配对码条带包围盒回归（#19）：PAIRBAND_BOX 只按提示文字宽度算时，6 位数字
 * 两端各被裁掉一位，电视上只剩中间 4 位。
 *
 * 直接 #include pairband.c 以访问静态着色器 pair_shade：对 0-9 每个数字
 * （放在每个位置）和最坏码 888888，扫描整条带，断言所有点亮像素都在
 * PAIRBAND_BOX 内；提示文字的点亮像素同样要在框内。并断言编码结果装得进 BAND_CAP。
 *
 * 用法：pairbox_test <blank.jpg>
 */
#include "../../main/pairband.c"

#include <stdio.h>
#include <stdlib.h>

static jb_page_t s_page;
static uint8_t s_band[BAND_CAP];
static size_t s_len;

uint8_t *stream_band_begin(void) { return s_band; }
size_t stream_band_cap(void) { return sizeof s_band; }
bool stream_band_publish(uint8_t *buf, size_t len, int page)
{
    (void)buf; (void)page;
    s_len = len;
    return true;
}
const jb_page_t *stream_page(int idx) { (void)idx; return &s_page; }

static int fails;

/* 扫整条带全宽，返回框外点亮像素数；同时统计框内点亮数。 */
static int outside(const char *code, int *inside)
{
    int out = 0;
    *inside = 0;
    for (int y = PAIR_ROW0 * 16; y < (PAIR_ROW0 + PAIR_ROWS) * 16; y++)
        for (int x = 0; x < 1920; x++) {
            if (!pair_shade(x, y, (void *)code)) continue;
            if (x >= PAIRBAND_BOX.x0 && x < PAIRBAND_BOX.x1 &&
                y >= PAIRBAND_BOX.y0 && y < PAIRBAND_BOX.y1)
                (*inside)++;
            else
                out++;
        }
    return out;
}

static void check(const char *code)
{
    int in_n, out_n = outside(code, &in_n);
    pairband_show(code);
    printf("码 %s：框内亮点 %d，框外亮点 %d，带长 %zu（BAND_CAP %d）\n",
           code, in_n, out_n, s_len, BAND_CAP);
    if (out_n) { fprintf(stderr, "  ✗ 有点亮像素落在 PAIRBAND_BOX 外\n"); fails++; }
    if (!s_len || s_len > BAND_CAP) { fprintf(stderr, "  ✗ 带长超 BAND_CAP\n"); fails++; }
}

int main(int argc, char **argv)
{
    if (argc != 2) { fprintf(stderr, "用法：%s <blank.jpg>\n", argv[0]); return 2; }
    FILE *f = fopen(argv[1], "rb");
    if (!f) return 2;
    fseek(f, 0, SEEK_END); long l = ftell(f); fseek(f, 0, SEEK_SET);
    unsigned char *b = malloc((size_t)l);
    if (fread(b, 1, (size_t)l, f) != (size_t)l) return 2;
    fclose(f);
    if (!jb_parse(&s_page, b, (size_t)l)) { fprintf(stderr, "底图不可用\n"); return 1; }

    /* 框必须覆盖第一位数字的左缘、最后一位数字的右缘、提示文字两端。 */
    if (PAIRBAND_BOX.x0 > X0 || PAIRBAND_BOX.x1 < DIG_X1 ||
        PAIRBAND_BOX.x0 > HINT_X0 || PAIRBAND_BOX.x1 < HINT_X0 + HINT_W) {
        fprintf(stderr, "✗ PAIRBAND_BOX x∈[%d,%d) 未覆盖提示文字与数字的并集\n",
                PAIRBAND_BOX.x0, PAIRBAND_BOX.x1);
        fails++;
    }

    /* 每个数字依次放在 6 个位置：d 在第 p 位，其余位用 8 垫满。 */
    for (int d = 0; d <= 9; d++)
        for (int p = 0; p < NDIG; p++) {
            char code[NDIG + 1] = "888888";
            code[p] = (char)('0' + d);
            int in_n, out_n = outside(code, &in_n);
            if (out_n) {
                fprintf(stderr, "✗ 码 %s：%d 个亮点在框外\n", code, out_n);
                fails++;
            }
        }
    /* 最坏码、每个数字的整码（000000..999999）、用户报的 776031。 */
    check("888888");
    for (int d = 0; d <= 9; d++) {
        char code[NDIG + 1];
        for (int i = 0; i < NDIG; i++) code[i] = (char)('0' + d);
        check(code);
    }
    check("776031");
    if (fails) { fprintf(stderr, "✗ %d 项失败\n", fails); return 1; }
    printf("✓ 所有亮点都在 PAIRBAND_BOX x∈[%d,%d) y∈[%d,%d) 内\n",
           PAIRBAND_BOX.x0, PAIRBAND_BOX.x1, PAIRBAND_BOX.y0, PAIRBAND_BOX.y1);
    return 0;
}
