/*
 * 槽头版本字段 → C 字符串。
 *
 * 槽头给版本留了 12 字节，历史上写入方只 memcpy 实际长度，剩余字节是
 * 擦除态 0xFF（不是 NUL）。直接按 12 字节 memcpy 再补 0，日志里就会打出
 * 「17����」，字符串比较也会带上这些不可见字节。
 *
 * 这里按「最多 12 字节、遇到 NUL 或不可打印字节就停」解码，然后补 0。
 * 版本号约定是 ASCII（当前是十进制数字串），遇到不可打印字符说明该槽
 * 是旧格式或已经损坏，截断比把垃圾读进比较逻辑安全。
 */
#include <stddef.h>
#include <string.h>

#include "framepack.h"

void fp_slot_ver(char *out, size_t cap, const uint8_t *field)
{
    if (!out || cap == 0) return;
    size_t n = 0;
    while (n < 12 && n + 1 < cap) {
        uint8_t c = field[n];
        if (c < 0x20 || c > 0x7e) break;
        out[n] = (char)c;
        n++;
    }
    out[n] = 0;
}
