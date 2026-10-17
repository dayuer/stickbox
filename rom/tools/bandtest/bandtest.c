/*
 * 时钟带拼接的主机侧验证。
 *
 * 拿 T13a 产出的黄金帧包（base.jpg + atlas.bin + meta.json）跑设备端的
 * fp_build_band()，再和 T13a 独立拼出来的 spliced.jpg 里对应的字节逐一
 * 比对。两个独立实现对上同一串字节，这套字节规则才算两边理解一致——
 * 没有板子也能把拼接逻辑钉死。
 *
 * 用法：bandtest <meta.json> <base.jpg> <atlas.bin> <spliced.jpg> HH MM
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "framepack.h"

static unsigned char *slurp(const char *path, size_t *len)
{
    FILE *f = fopen(path, "rb");
    if (!f) { fprintf(stderr, "打不开 %s\n", path); exit(2); }
    fseek(f, 0, SEEK_END); long n = ftell(f); fseek(f, 0, SEEK_SET);
    unsigned char *b = malloc((size_t)n);
    if (fread(b, 1, (size_t)n, f) != (size_t)n) { fprintf(stderr, "读 %s 失败\n", path); exit(2); }
    fclose(f);
    *len = (size_t)n;
    return b;
}

int main(int argc, char **argv)
{
    if (argc != 7) {
        fprintf(stderr, "用法：%s <meta.json> <base.jpg> <atlas.bin> <spliced.jpg> HH MM\n", argv[0]);
        return 2;
    }
    size_t mlen, blen, alen, slen;
    char *meta = (char *)slurp(argv[1], &mlen);
    unsigned char *base = slurp(argv[2], &blen);
    unsigned char *atlas = slurp(argv[3], &alen);
    unsigned char *spliced = slurp(argv[4], &slen);
    int hh = atoi(argv[5]), mm = atoi(argv[6]);

    char *metaz = malloc(mlen + 1);
    memcpy(metaz, meta, mlen); metaz[mlen] = 0;

    fp_index_t ix;
    if (!fp_parse_meta(metaz, mlen, &ix)) { fprintf(stderr, "meta 解析失败\n"); return 1; }
    const char *why;
    if (!fp_index_valid(&ix, blen, alen, 16 * 1024, &why)) {
        fprintf(stderr, "索引不合法：%s\n", why); return 1;
    }

    static unsigned char out[64 * 1024];
    size_t n = fp_build_band(&ix, base, blen, atlas, alen, hh, mm, out, sizeof out);
    if (!n) { fprintf(stderr, "拼接失败\n"); return 1; }

    uint32_t off[FP_BAND_ROWS], len[FP_BAND_ROWS];
    fp_band_layout(&ix, off, len, hh, mm);

    printf("拼接 %02d:%02d → %zu 字节（max_band_bytes=%u）\n", hh, mm, n, ix.max_band_bytes);
    if (n > ix.max_band_bytes) {
        printf("✗ 超过服务端声明的最坏值\n");
        return 1;
    }

    /* 和黄金 spliced.jpg 比：逐行找到该行在 spliced 里的位置。
     * 行在 spliced 里的偏移 = 底帧里的 row_run.off 加上前面各行的长度变化。 */
    long delta = 0;
    int bad = 0;
    for (int row = 0; row < FP_BAND_ROWS; row++) {
        long want_off = (long)ix.row_runs[row].off + delta;
        if (want_off < 0 || (size_t)want_off + len[row] > slen) {
            printf("✗ 行%d 超出 spliced 范围\n", row);
            bad = 1; break;
        }
        if (memcmp(out + off[row], spliced + want_off, len[row]) != 0) {
            printf("✗ 行%d 不一致（本地 %u 字节 @%u，黄金 @%ld）\n",
                   row, len[row], off[row], want_off);
            bad = 1;
        }
        delta += (long)len[row] - (long)ix.row_runs[row].len;
    }
    if (bad) return 1;
    printf("✓ 12 行与 T13a 的 spliced.jpg 逐字节一致\n");
    return 0;
}
