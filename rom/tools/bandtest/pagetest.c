/*
 * 把四种状态页渲染成 JPEG，用肉眼和 djpeg 验一次。
 *
 * 为什么要这个：状态页是「出故障时电视上唯一还能显示的东西」。它渲染错了
 * 不会报错，只会在电视上变成一条看不懂的横带——而这正是上一版实际发生的
 * 事（空白带 + 蓝底色，两个 bug 叠在一起没人看得出来）。
 *
 * 这里走的是设备上真正跑的那条路：同一份 statusmask、同一个 jb_encode_rows、
 * 同一个 statusband 的着色器。区别只是把结果写成文件而不是推给电视。
 *
 * 用法：pagetest <blank.jpg> <输出目录>
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "jband.h"
#include "pairband.h"
#include "statusmask.h"

#define BG 0x1D2D3D
#define FG 0xF5F5F8

static const status_mask_t *s_mask;
static int s_x0, s_y0;

/* 和 statusband.c 的着色器逐行相同。放在这里是因为那个是 static——
 * 复制一份比把它导出去污染设备侧接口要好，但两者必须保持同步，
 * 改了 statusband.c 的着色器记得同步这里。 */
static bool shade(int x, int y, void *ctx)
{
    (void)ctx;
    const status_mask_t *m = s_mask;
    if (!m) return false;
    int mx = x - s_x0, my = y - s_y0;
    if (mx < 0 || my < 0 || mx >= m->w || my >= m->h) return false;
    return (m->bits[my * m->stride + (mx >> 3)] >> (7 - (mx & 7))) & 1;
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

static const char *NAMES[STATUS_COUNT] = {
    "connecting", "fetching", "offline", "wait_pair", "no_server",
};

int main(int argc, char **argv)
{
    if (argc != 3) {
        fprintf(stderr, "用法：%s <blank.jpg> <输出目录>\n", argv[0]);
        return 2;
    }
    size_t blen;
    unsigned char *blank = slurp(argv[1], &blen);

    jb_page_t page;
    if (!jb_parse(&page, blank, blen)) {
        fprintf(stderr, "底图不是本工程能用的 JPEG（要 baseline/4:2:0/两张量化表/DRI=每行）\n");
        return 1;
    }
    printf("底图 %dx%d，%d×%d MCU，DRI=每行\n", page.w, page.h, page.mcu_cols, page.mcu_rows);

    static uint8_t band[64 * 1024];
    int fails = 0;

    for (int id = 0; id < STATUS_COUNT; id++) {
        const status_mask_t *m = &STATUS_MASKS[id];
        s_mask = m;
        s_x0 = (1920 - m->w) / 2;
        s_y0 = PAIR_ROW0 * 16 + (PAIR_ROWS * 16 - m->h) / 2;

        const jb_box_t box = { s_x0, s_y0, s_x0 + m->w, s_y0 + m->h };
        size_t n = jb_encode_rows(&page, PAIR_ROW0, PAIR_ROW0 + PAIR_ROWS,
                                  BG, FG, shade, NULL, &box, band, 16 * 1024);
        if (!n) {
            printf("✗ %-11s 编码失败（超出 16KB 缓冲？）\n", NAMES[id]);
            fails++;
            continue;
        }

        /* 按推流的三段拼接拼出整帧，写成文件。 */
        size_t a = page.row_off[PAIR_ROW0];
        size_t b = page.row_off[PAIR_ROW0 + PAIR_ROWS];
        char path[512];
        snprintf(path, sizeof path, "%s/page-%s.jpg", argv[2], NAMES[id]);
        FILE *f = fopen(path, "wb");
        if (!f) { fprintf(stderr, "写不了 %s\n", path); return 2; }
        fwrite(blank, 1, a, f);
        fwrite(band, 1, n, f);
        fwrite(blank + b, 1, blen - b, f);
        size_t total = a + n + (blen - b);
        fclose(f);

        printf("✓ %-11s 文字 %u×%u，带 %zu 字节，整帧 %zu 字节 → %s\n",
               NAMES[id], m->w, m->h, n, total, path);
    }
    return fails ? 1 : 0;
}
