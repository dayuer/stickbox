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

/* 极简 JSON 取数：找到 "key" 之后的第一个数字。meta.json 是服务端生成的，
 * 结构固定，这里只为测试，不做通用解析。 */
static const char *find_key(const char *s, const char *key)
{
    char pat[64];
    snprintf(pat, sizeof pat, "\"%s\"", key);
    return strstr(s, pat);
}

static long num_after(const char **p)
{
    const char *s = *p;
    while (*s && (*s < '0' || *s > '9') && *s != '-') s++;
    char *end;
    long v = strtol(s, &end, 10);
    *p = end;
    return v;
}

/* 从 "runs":[{"off":N,"len":M},...] 里读 count 段。 */
static void read_runs(const char **p, fp_run_t *out, int count)
{
    const char *s = find_key(*p, "runs");
    if (!s) { fprintf(stderr, "缺 runs\n"); exit(2); }
    s += 6;
    for (int i = 0; i < count; i++) {
        const char *o = find_key(s, "off"); const char *l;
        if (!o) { fprintf(stderr, "runs 段不够\n"); exit(2); }
        o += 5; out[i].off = (uint32_t)num_after(&o);
        l = find_key(o, "len"); l += 5; out[i].len = (uint32_t)num_after(&l);
        s = l;
    }
    *p = s;
}

static void parse_meta(const char *json, fp_index_t *ix)
{
    memset(ix, 0, sizeof *ix);
    ix->band_row0 = 4;
    ix->intervals_per_row = 15;

    const char *p = find_key(json, "dri");
    if (p) { p += 5; long dri = num_after(&p);
             const char *c = find_key(json, "mcu_cols");
             if (c) { c += 10; long cols = num_after(&c); ix->intervals_per_row = (uint16_t)(cols / dri); } }

    p = find_key(json, "rows");
    if (p) { p += 6; ix->band_row0 = (uint16_t)num_after(&p); }

    p = find_key(json, "max_band_bytes");
    if (p) { p += 16; ix->max_band_bytes = (uint32_t)num_after(&p); }

    p = find_key(json, "row_runs");
    if (!p) { fprintf(stderr, "缺 row_runs\n"); exit(2); }
    p += 10;
    for (int i = 0; i < FP_BAND_ROWS; i++) {
        const char *o = find_key(p, "off"); o += 5; ix->row_runs[i].off = (uint32_t)num_after(&o);
        const char *l = find_key(o, "len"); l += 5; ix->row_runs[i].len = (uint32_t)num_after(&l);
        p = l;
    }

    p = find_key(json, "slots");
    if (!p) { fprintf(stderr, "缺 slots\n"); exit(2); }
    for (int s = 0; s < FP_SLOTS; s++) {
        const char *mc = find_key(p, "mcu_col");
        if (!mc) { fprintf(stderr, "slots 不够\n"); exit(2); }
        mc += 9; ix->slot_mcu_col[s] = (uint16_t)num_after(&mc);
        p = mc;
        read_runs(&p, ix->slot_runs[s], FP_BAND_ROWS);
    }

    p = find_key(json, "digits");
    if (!p) { fprintf(stderr, "缺 digits\n"); exit(2); }
    for (int d = 0; d < FP_DIGITS; d++) {
        const char *dd = find_key(p, "\"d\"");
        if (!dd) dd = p;
        p = dd;
        read_runs(&p, ix->digit_runs[d], FP_BAND_ROWS);
    }
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
    parse_meta(metaz, &ix);

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
