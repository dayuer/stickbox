/*
 * 端到端：吃一个真实的 SBF1 帧包，走设备侧的全部字节逻辑，
 * 和服务端的黄金拼接帧逐字节比对。
 *
 * 这一步补的是 bandtest 没覆盖的那段：**SBF1 容器的解析**。bandtest 吃的是
 * 已经拆开的 base/atlas/meta 三个文件，而设备拿到的是一个容器——容器头算错
 * 一个字节，后面全错，而且在板上表现为花屏，查起来很贵。
 *
 * 容器布局（server/go/internal/frames/frames.go 的 Pack，全大端）：
 *   [0:4]   "SBF1"
 *   [4:8]   base_len
 *   [8:12]  atlas_len
 *   [12:16] meta_len
 *   [16:]   base.jpg, atlas.bin, meta.json
 *
 * 用法：packtest <frame.sbf1> <golden-HHMM.jpg> HH MM
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "framepack.h"

#define PACK_HEADER 16

static unsigned char *slurp(const char *p, size_t *n)
{
    FILE *f = fopen(p, "rb");
    if (!f) { fprintf(stderr, "打不开 %s\n", p); exit(2); }
    fseek(f, 0, SEEK_END); long l = ftell(f); fseek(f, 0, SEEK_SET);
    unsigned char *b = malloc((size_t)l);
    if (fread(b, 1, (size_t)l, f) != (size_t)l) { fprintf(stderr, "读 %s 失败\n", p); exit(2); }
    fclose(f);
    *n = (size_t)l;
    return b;
}

static uint32_t be32(const unsigned char *p)
{
    return ((uint32_t)p[0] << 24) | ((uint32_t)p[1] << 16) | ((uint32_t)p[2] << 8) | p[3];
}

int main(int argc, char **argv)
{
    if (argc != 5) {
        fprintf(stderr, "用法：%s <frame.sbf1> <golden.jpg> HH MM\n", argv[0]);
        return 2;
    }
    size_t plen, glen;
    unsigned char *pk = slurp(argv[1], &plen);
    unsigned char *golden = slurp(argv[2], &glen);
    int hh = atoi(argv[3]), mm = atoi(argv[4]);

    /* ---- 容器 ---- */
    if (plen <= PACK_HEADER || memcmp(pk, "SBF1", 4) != 0) {
        printf("✗ 容器 magic 不对\n");
        return 1;
    }
    uint32_t bl = be32(pk + 4), al = be32(pk + 8), ml = be32(pk + 12);
    if ((uint64_t)PACK_HEADER + bl + al + ml != plen) {
        printf("✗ 三段长度 %u+%u+%u+16 = %llu，容器实际 %zu\n",
               bl, al, ml, (unsigned long long)PACK_HEADER + bl + al + ml, plen);
        return 1;
    }
    const unsigned char *base = pk + PACK_HEADER;
    const unsigned char *atlas = base + bl;
    const char *meta = (const char *)(atlas + al);
    printf("容器：底帧 %u、图集 %u、meta %u，合计 %zu\n", bl, al, ml, plen);

    if (plen > FP_MAX_PACK) {
        printf("✗ 帧包 %zu 超过槽容量 %d\n", plen, FP_MAX_PACK);
        return 1;
    }

    /* ---- 索引 ---- */
    fp_index_t ix;
    if (!fp_parse_meta(meta, ml, &ix)) { printf("✗ meta 解析失败\n"); return 1; }
    const char *why;
    if (!fp_index_valid(&ix, bl, al, 16 * 1024, &why)) {
        printf("✗ 索引不合法：%s\n", why);
        return 1;
    }
    printf("索引：max_band_bytes=%u（BAND_CAP 16384）\n", ix.max_band_bytes);

    /* ---- 拼接 ---- */
    static unsigned char band[64 * 1024];
    size_t n = fp_build_band(&ix, base, bl, atlas, al, hh, mm, band, 16 * 1024);
    if (!n) { printf("✗ 拼 %02d:%02d 失败\n", hh, mm); return 1; }
    if (n > ix.max_band_bytes) {
        printf("✗ 实拼 %zu 超过服务端声明的最坏值 %u\n", n, ix.max_band_bytes);
        return 1;
    }

    uint32_t off[FP_BAND_ROWS], len[FP_BAND_ROWS];
    fp_band_layout(&ix, off, len, hh, mm);

    /* ---- 整帧段序列，和推流发出去的字节完全一致 ---- */
    fp_seg_t segs[FP_MAX_SEGS];
    size_t total = 0;
    int ns = fp_frame_segments(&ix, base, bl, band, off, len, segs, FP_MAX_SEGS, &total);
    if (!ns) { printf("✗ 段编排失败\n"); return 1; }
    if (total != glen) {
        printf("✗ 整帧 %zu 字节，黄金 %zu 字节\n", total, glen);
        return 1;
    }
    size_t at = 0;
    for (int i = 0; i < ns; i++) {
        if (memcmp(segs[i].p, golden + at, segs[i].n) != 0) {
            printf("✗ 第 %d/%d 段不一致（偏移 %zu，%zu 字节）\n", i + 1, ns, at, segs[i].n);
            return 1;
        }
        at += segs[i].n;
    }
    printf("✓ %02d:%02d：带 %zu 字节，整帧 %d 段 %zu 字节，与服务端黄金帧逐字节一致\n",
           hh, mm, n, ns, total);
    return 0;
}
