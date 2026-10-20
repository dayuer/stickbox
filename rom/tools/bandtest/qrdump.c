/*
 * 配对页二维码端到端产物：走设备上同一条 pairband_show -> jb_encode_rows，
 * 把带按推流的三段拼接方式拼回底图，写成完整 JPEG，并打印带长。
 * 之后由 qrdecode.py 用独立解码器（zxing-cpp）从这张 JPEG 里解出 URL，
 * 验证的是 JPEG 量化之后的二维码，不是理想位图。
 *
 * 带二维码的带走临时大缓冲（PAIR_BAND_CAP），与设备一致。
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

#include "streamstub.h"

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
    fwrite(s_pub_extra ? s_extra_mem : s_band, 1, s_len, o);
    fwrite(blank + b, 1, (size_t)l - b, o);
    fclose(o);
    printf("%s %zu\n", argv[2], s_len);
    return 0;
}
