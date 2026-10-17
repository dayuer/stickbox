/* meta 解析与索引校验的拒绝路径。输入来自网络，坏输入必须被挡在加载阶段。 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "framepack.h"

static char *slurp(const char *p, size_t *n)
{
    FILE *f = fopen(p, "rb"); if (!f) { perror(p); exit(2); }
    fseek(f, 0, SEEK_END); long l = ftell(f); fseek(f, 0, SEEK_SET);
    char *b = malloc(l + 1); fread(b, 1, l, f); b[l] = 0; fclose(f);
    *n = (size_t)l; return b;
}

static int fails = 0;
static void expect(const char *name, bool got, bool want)
{
    printf("%-34s %s\n", name, got == want ? "PASS" : "FAIL");
    if (got != want) fails++;
}

int main(int argc, char **argv)
{
    if (argc != 2) { fprintf(stderr, "用法：metatest <meta.json>\n"); return 2; }
    size_t n; char *good = slurp(argv[1], &n);
    fp_index_t ix;

    expect("完整 meta 能解析", fp_parse_meta(good, n, &ix), true);

    /* 截断：任何位置截断都不能解析成功后留下半套索引。 */
    bool any_bad = false;
    for (size_t cut = 1; cut < n; cut += 97) {
        fp_index_t t;
        if (fp_parse_meta(good, cut, &t)) {
            /* 允许解析成功，但索引必须过不了自洽检查。 */
            const char *why;
            if (fp_index_valid(&t, 1 << 20, 1 << 20, 16 * 1024, &why)) any_bad = true;
        }
    }
    expect("任意位置截断都不会产出可用索引", any_bad, false);

    expect("空输入被拒", fp_parse_meta("", 0, &ix), false);
    expect("非 JSON 被拒", fp_parse_meta("hello", 5, &ix), false);

    /* max_band_bytes 超过本机缓冲 → 整包拒绝。 */
    fp_parse_meta(good, n, &ix);
    const char *why;
    expect("索引自洽（16KB 缓冲）", fp_index_valid(&ix, 1 << 20, 1 << 20, 16 * 1024, &why), true);
    expect("缓冲只有 8KB 时拒绝整包",
           fp_index_valid(&ix, 1 << 20, 1 << 20, 8 * 1024, &why), false);

    /* 底帧/图集比索引声明的小 → 拒绝，不能越界读。 */
    expect("底帧过小被拒", fp_index_valid(&ix, 1024, 1 << 20, 16 * 1024, &why), false);
    expect("图集过小被拒", fp_index_valid(&ix, 1 << 20, 1024, 16 * 1024, &why), false);

    /* 槽不按列升序 → 拒绝（拼接时游标会倒退）。 */
    fp_index_t sw = ix;
    for (int r = 0; r < FP_BAND_ROWS; r++) {
        fp_run_t t = sw.slot_runs[0][r];
        sw.slot_runs[0][r] = sw.slot_runs[3][r];
        sw.slot_runs[3][r] = t;
    }
    expect("槽顺序被打乱后拒绝", fp_index_valid(&sw, 1 << 20, 1 << 20, 16 * 1024, &why), false);

    /* 非法时刻不拼。 */
    static unsigned char out[32 * 1024];
    unsigned char base[1 << 20], atlas[1 << 20];
    memset(base, 0, sizeof base); memset(atlas, 0, sizeof atlas);
    expect("hh=24 不拼", fp_build_band(&ix, base, sizeof base, atlas, sizeof atlas,
                                       24, 0, out, sizeof out) == 0, true);
    expect("mm=60 不拼", fp_build_band(&ix, base, sizeof base, atlas, sizeof atlas,
                                       0, 60, out, sizeof out) == 0, true);
    expect("负数不拼", fp_build_band(&ix, base, sizeof base, atlas, sizeof atlas,
                                     -1, 0, out, sizeof out) == 0, true);
    /* 输出缓冲不够时宁可不拼，不能越界写。 */
    expect("cap 不够时返回 0", fp_build_band(&ix, base, sizeof base, atlas, sizeof atlas,
                                            12, 47, out, 100) == 0, true);

    printf("\n%s（%d 条失败）\n", fails ? "有失败" : "全部通过", fails);
    return fails ? 1 : 0;
}
