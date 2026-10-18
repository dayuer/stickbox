/*
 * 播放清单流式解析（M2）。实现说明见 photo_json.h。
 *
 * 这是一个只服务清单文档的宽松 JSON 解析器：结构按 JSON 校验（括号、
 * 字符串、数字），但只对认识的键取值，其余值一律跳过。输入来自网络，
 * 任何不合规都整份拒绝，调用方不会拿到半份清单。
 */
#include "photo_json.h"

#include <limits.h>
#include <stdlib.h>
#include <string.h>

#define PHOTO_MAX_BYTES (16u * 1024 * 1024)

bool photo_sha_from_hex(const char hex[PHOTO_PL_HEX], uint8_t out[32])
{
    for (int i = 0; i < 32; i++) {
        int hi, lo;
        char a = hex[i * 2], b = hex[i * 2 + 1];
        if (a >= '0' && a <= '9') hi = a - '0';
        else if (a >= 'a' && a <= 'f') hi = a - 'a' + 10;
        else return false;
        if (b >= '0' && b <= '9') lo = b - '0';
        else if (b >= 'a' && b <= 'f') lo = b - 'a' + 10;
        else return false;
        out[i] = (uint8_t)((hi << 4) | lo);
    }
    return true;
}

static bool is_ws(unsigned char c)
{
    return c == ' ' || c == '\t' || c == '\r' || c == '\n';
}

static bool is_digit(unsigned char c) { return c >= '0' && c <= '9'; }

static bool is_num_char(unsigned char c)
{
    return is_digit(c) || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E';
}

static pj_ctx_t cur_ctx(const photo_json_t *p)
{
    return p->sp ? p->ctx[p->sp - 1] : PJ_CTX_NONE;
}

static bool ctx_push(photo_json_t *p, pj_ctx_t c, bool is_arr)
{
    if (p->sp >= (int)(sizeof p->ctx / sizeof p->ctx[0])) return false;
    p->ctx[p->sp] = c;
    p->is_arr[p->sp] = is_arr;
    p->sp++;
    return true;
}

static void clear_key(photo_json_t *p)
{
    p->have_key = false;
    p->key[0] = 0;
}

static void str_putc(photo_json_t *p, unsigned char c)
{
    if (p->str_len < (int)sizeof p->str - 1) p->str[p->str_len++] = (char)c;
    else p->str_long = true;
}

static void str_putcp(photo_json_t *p, uint32_t cp)
{
    if (cp < 0x80) {
        str_putc(p, (unsigned char)cp);
    } else if (cp < 0x800) {
        str_putc(p, (unsigned char)(0xC0 | (cp >> 6)));
        str_putc(p, (unsigned char)(0x80 | (cp & 0x3F)));
    } else if (cp >= 0xD800 && cp <= 0xDFFF) {
        str_putc(p, '?');   /* 代理对：清单里不会出现，跳过即可 */
    } else {
        str_putc(p, (unsigned char)(0xE0 | (cp >> 12)));
        str_putc(p, (unsigned char)(0x80 | ((cp >> 6) & 0x3F)));
        str_putc(p, (unsigned char)(0x80 | (cp & 0x3F)));
    }
}

static void reset_entry(photo_json_t *p)
{
    p->kind[0] = 0;
    p->etheme[0] = 0;
    p->esha[0] = 0;
    p->esize = 0;
    p->esize_set = false;
    p->edur = 0;
    p->eorder = 0;
    p->efield_bad = false;
}

/* ------------------------------------------------------------ 值派发 */

static bool deliver_str(photo_json_t *p)
{
    pj_ctx_t c = cur_ctx(p);
    int n = p->str_len;
    p->str[n] = 0;

    if (c == PJ_CTX_TOP) {
        if (p->have_key && strcmp(p->key, "init_token") == 0) {
            if (p->str_long || n > 64) return false;
            memcpy(p->out->init_token, p->str, (size_t)n + 1);
            p->out->has_token = n > 0;
        }
    } else if (c == PJ_CTX_ENTRY) {
        if (p->have_key && strcmp(p->key, "kind") == 0) {
            /* 过长/含转义的 kind 一定是未知 kind：留空按忽略处理。 */
            if (!p->str_long && n <= 15) memcpy(p->kind, p->str, (size_t)n + 1);
            else p->kind[0] = 0;
        } else if (p->have_key && strcmp(p->key, "sha256") == 0) {
            /* 只有 photo 条目需要合法 sha；未知 kind 的字段内容不校验，
             * 但也不能把非法 sha 当有效值留给 finalize。 */
            if (!p->str_long && n == PHOTO_PL_HEX)
                memcpy(p->esha, p->str, PHOTO_PL_HEX + 1);
            else p->esha[0] = 0;
        } else if (p->have_key && strcmp(p->key, "theme") == 0) {
            if (!p->str_long && n < (int)sizeof p->etheme)
                memcpy(p->etheme, p->str, (size_t)n + 1);
            else p->etheme[0] = 0;
        }
    }
    clear_key(p);
    return true;
}

static bool deliver_num(photo_json_t *p)
{
    pj_ctx_t c = cur_ctx(p);
    p->num[p->num_len] = 0;

    bool known = false;
    if (c == PJ_CTX_TOP && p->have_key) {
        known = strcmp(p->key, "plv") == 0 ||
                strcmp(p->key, "init_expires_at") == 0;
    } else if (c == PJ_CTX_ENTRY && p->have_key) {
        known = strcmp(p->key, "size") == 0 ||
                strcmp(p->key, "duration_s") == 0 ||
                strcmp(p->key, "order") == 0;
    }
    /* 未知字段里的数值（包括小数/指数）一律跳过，不能因此整份拒绝；
     * 认识的字段必须是整数。 */
    if (!known) {
        clear_key(p);
        return true;
    }
    if (strpbrk(p->num, ".eE")) return false;
    char *end = NULL;
    long long v = strtoll(p->num, &end, 10);
    if (!end || *end || end == p->num) return false;

    if (c == PJ_CTX_TOP) {
        if (strcmp(p->key, "plv") == 0) {
            if (v < 0 || v > INT_MAX) return false;
            p->out->plv = (int)v;
            p->out->has_plv = true;
        } else {   /* init_expires_at */
            if (v <= 0) return false;
            p->out->expires_at = (int64_t)v;
            p->out->has_expires = true;
        }
    } else {
        if (strcmp(p->key, "size") == 0) {
            if (v <= 0 || v > (long long)PHOTO_MAX_BYTES) return false;
            p->esize = (uint32_t)v;
            p->esize_set = true;
        } else if (strcmp(p->key, "duration_s") == 0) {
            if (v < 0 || v > 600) return false;
            p->edur = (uint32_t)v;
        } else {   /* order */
            if (v < 0 || v > 65535) return false;
            p->eorder = (uint32_t)v;
        }
    }
    clear_key(p);
    return true;
}

static bool deliver_lit(photo_json_t *p)
{
    pj_ctx_t c = cur_ctx(p);
    p->lit[p->lit_len] = 0;
    bool is_true = strcmp(p->lit, "true") == 0;
    bool is_false = strcmp(p->lit, "false") == 0;
    bool is_null = strcmp(p->lit, "null") == 0;
    if (!is_true && !is_false && !is_null) return false;

    if (c == PJ_CTX_TOP && p->have_key &&
        strcmp(p->key, "init_force") == 0) {
        p->out->init_force = is_true;
    }
    clear_key(p);
    return true;
}

static bool finalize_entry(photo_json_t *p)
{
    if (strcmp(p->kind, "board") == 0) {
        /* 服务端保证只有一条 board；重复出现时保留第一条。 */
        if (!p->out->board_seen) {
            p->out->board_seen = true;
            p->out->board_duration_s = (uint16_t)p->edur;
            memcpy(p->out->board_theme, p->etheme, sizeof p->out->board_theme);
            p->out->board_theme[sizeof p->out->board_theme - 1] = 0;
        }
        return true;
    }
    if (strcmp(p->kind, "photo") != 0) return true;   /* 未知 kind 忽略 */

    if (p->esha[0] == 0 || !p->esize_set || p->esize == 0 ||
        p->esize > PHOTO_MAX_BYTES) return false;
    uint8_t bin[32];
    if (!photo_sha_from_hex(p->esha, bin)) return false;
    if (p->out->photo_count >= PHOTO_PL_MAX_PHOTOS) return false;

    photo_pl_entry_t *e = &p->out->photos[p->out->photo_count++];
    memcpy(e->sha256, bin, sizeof bin);
    e->size = p->esize;
    e->duration_s = (uint16_t)p->edur;
    e->order = (uint16_t)p->eorder;
    return true;
}

/* ------------------------------------------------------------ 结构 */

static bool handle_struct(photo_json_t *p, unsigned char c)
{
    switch (c) {
    case '{': {
        pj_ctx_t nc = PJ_CTX_SKIP;
        if (p->sp == 0) {
            nc = PJ_CTX_TOP;
        } else if (cur_ctx(p) == PJ_CTX_ENTRIES) {
            nc = PJ_CTX_ENTRY;
            reset_entry(p);
            if (++p->out->entry_count > PHOTO_PL_MAX_ENTRIES) return false;
        }
        if (!ctx_push(p, nc, false)) return false;
        clear_key(p);
        return true;
    }
    case '[': {
        pj_ctx_t nc = PJ_CTX_SKIP;
        if (cur_ctx(p) == PJ_CTX_TOP && p->have_key &&
            strcmp(p->key, "entries") == 0) {
            nc = PJ_CTX_ENTRIES;
            p->out->entries_seen = true;
        }
        if (!ctx_push(p, nc, true)) return false;
        clear_key(p);
        return true;
    }
    case '}':
    case ']': {
        bool is_arr = c == ']';
        if (p->sp == 0 || p->is_arr[p->sp - 1] != is_arr) return false;
        pj_ctx_t c2 = p->ctx[p->sp - 1];
        p->sp--;
        if (c2 == PJ_CTX_ENTRY && !finalize_entry(p)) return false;
        clear_key(p);
        if (c2 == PJ_CTX_TOP) p->done = true;
        return true;
    }
    case ',':
        clear_key(p);
        return true;
    default:
        return false;
    }
}

/* ------------------------------------------------------------ 字节机 */

static bool begin_string(photo_json_t *p)
{
    p->in_str = true;
    p->esc = 0;
    p->str_len = 0;
    p->str_long = false;
    p->str[0] = 0;
    return true;
}

static bool string_byte(photo_json_t *p, unsigned char c)
{
    if (p->esc == 1) {
        p->esc = 0;
        switch (c) {
        case '"':  str_putc(p, '"');  break;
        case '\\': str_putc(p, '\\'); break;
        case '/':  str_putc(p, '/');  break;
        case 'b':  str_putc(p, '\b'); break;
        case 'f':  str_putc(p, '\f'); break;
        case 'n':  str_putc(p, '\n'); break;
        case 'r':  str_putc(p, '\r'); break;
        case 't':  str_putc(p, '\t'); break;
        case 'u':
            p->esc = 2;
            p->ucode = 0;
            break;
        default:
            return false;
        }
        return true;
    }
    if (p->esc >= 2) {
        int v;
        if (c >= '0' && c <= '9') v = c - '0';
        else if (c >= 'a' && c <= 'f') v = c - 'a' + 10;
        else if (c >= 'A' && c <= 'F') v = c - 'A' + 10;
        else return false;
        p->ucode = (p->ucode << 4) | (uint32_t)v;
        if (++p->esc == 6) {
            str_putcp(p, p->ucode);
            p->esc = 0;
        }
        return true;
    }
    if (c == '\\') {
        p->esc = 1;
        return true;
    }
    if (c == '"') {
        p->in_str = false;
        p->pend_str = true;
        return true;
    }
    if (c < 0x20) return false;   /* 裸控制字符不是合法 JSON */
    str_putc(p, c);
    return true;
}

static bool process_byte(photo_json_t *p, unsigned char c)
{
    if (p->done) return is_ws(c);

    if (p->in_str) return string_byte(p, c);

    if (p->esc) return false;     /* 不会发生：esc 只在 in_str 里 */

    if (p->in_num) {
        if (is_num_char(c)) {
            if (p->num_len >= (int)sizeof p->num - 1) return false;
            p->num[p->num_len++] = (char)c;
            return true;
        }
        p->in_num = false;
        if (!deliver_num(p)) return false;
        /* 落到下面继续处理当前字符 */
    }

    if (p->in_lit) {
        if ((c >= 'a' && c <= 'z')) {
            if (p->lit_len >= (int)sizeof p->lit - 1) return false;
            p->lit[p->lit_len++] = (char)c;
            return true;
        }
        p->in_lit = false;
        if (!deliver_lit(p)) return false;
    }

    if (p->pend_str) {
        if (is_ws(c)) return true;
        if (c == ':') {
            clear_key(p);
            if (!p->str_long && p->str_len > 0 &&
                p->str_len < (int)sizeof p->key) {
                memcpy(p->key, p->str, (size_t)p->str_len);
                p->key[p->str_len] = 0;
                p->have_key = true;
            }
            p->pend_str = false;
            return true;
        }
        if (!deliver_str(p)) return false;
        p->pend_str = false;
    }

    if (is_ws(c)) return true;
    if (c == '"') return begin_string(p);
    if (c == '-' || is_digit(c)) {
        p->in_num = true;
        p->num_len = 0;
        if (p->num_len >= (int)sizeof p->num - 1) return false;
        p->num[p->num_len++] = (char)c;
        return true;
    }
    if (c == 't' || c == 'f' || c == 'n') {
        p->in_lit = true;
        p->lit_len = 0;
        p->lit[p->lit_len++] = (char)c;
        return true;
    }
    return handle_struct(p, c);
}

void photo_json_init(photo_json_t *p, photo_pl_t *out)
{
    memset(p, 0, sizeof *p);
    memset(out, 0, sizeof *out);
    p->out = out;
}

bool photo_json_feed(photo_json_t *p, const void *data, size_t len)
{
    if (!p || !p->out || p->err) return false;
    const uint8_t *d = data;
    for (size_t i = 0; i < len; i++) {
        if (!process_byte(p, d[i])) {
            p->err = true;
            return false;
        }
    }
    return true;
}

bool photo_json_finish(photo_json_t *p)
{
    if (!p || p->err || !p->done || p->sp != 0) return false;
    if (p->in_str || p->in_num || p->in_lit || p->pend_str) return false;
    return true;
}
