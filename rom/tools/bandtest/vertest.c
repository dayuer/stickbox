/*
 * 槽头版本字段解码单测。
 *
 * v0.9.5 上板日志出现「版本 17����」：写入方只 memcpy 实际长度，剩余
 * 字节是擦除态 0xFF；读取方按 12 字节拷进 char[] 再当 %s，打出了不可见
 * 字符，版本比较也会带上它们。这个测试钉死解码规则：
 *   - 最多 12 字节；
 *   - 遇到 NUL 或不可打印字节立即截断；
 *   - 输出始终以 NUL 结尾，空间不足时最多 cap-1 个字符。
 */
#include <stdio.h>
#include <string.h>

#include "framepack.h"

static int fails;

static void check(const char *name, const uint8_t field[12], size_t cap,
                  const char *want)
{
    char out[32];
    memset(out, 0xAA, sizeof out);
    fp_slot_ver(out, cap, field);
    if (strcmp(out, want) != 0) {
        printf("✗ %-24s 得到 \"%s\"，期望 \"%s\"\n", name, out, want);
        fails++;
        return;
    }
    /* 必须终止，且终止符在 cap 内。 */
    if (cap > 0 && out[cap - 1] != 0 && strlen(out) >= cap) {
        printf("✗ %-24s 没有在 cap 内终止\n", name);
        fails++;
        return;
    }
    printf("✓ %-24s \"%s\"\n", name, out);
}

int main(void)
{
    const uint8_t ff_pad[12] = { '1', '7', 0xFF, 0xFF, 0xFF, 0xFF,
                                 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF };
    const uint8_t nul_pad[12] = { '2', '2', 0, 0xFF, 0xFF, 0xFF,
                                  0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF };
    const uint8_t all_ff[12] = { 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
                                 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF };
    const uint8_t full[12] = { '1', '2', '3', '4', '5', '6',
                               '7', '8', '9', '0', '1', '2' };
    const uint8_t embedded[12] = { '1', '7', 0x01, '9', 0xFF, 0xFF,
                                   0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF };

    check("0xFF 填充", ff_pad, 32, "17");
    check("NUL 填充", nul_pad, 32, "22");
    check("全 0xFF", all_ff, 32, "");
    check("12 字节整串", full, 32, "123456789012");
    check("中间不可打印", embedded, 32, "17");
    check("缓冲区只有 4 字节", full, 4, "123");

    if (fails) {
        printf("%d 项失败\n", fails);
        return 1;
    }
    return 0;
}
