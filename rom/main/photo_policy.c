#include "photo_policy.h"

#include <stdio.h>
#include <string.h>

photo_fmt_action_t photo_format_action(photo_format_mode_t mode, bool has_token,
                                       bool token_used, bool time_valid,
                                       int64_t now, bool has_expires,
                                       int64_t expires, bool force_flag)
{
    if (mode == PHOTO_FMT_READY || mode == PHOTO_FMT_UNAVAILABLE)
        return PHOTO_FMT_NO;
    if (!has_token || token_used) return PHOTO_FMT_NO;
    /* 没有绝对到期时间就没法判断 10 分钟窗口；SNTP 没同步时本机时间
     * 也不可信。两种情况都宁可不动卡。 */
    if (!has_expires || !time_valid || now <= 0) return PHOTO_FMT_NO;
    if (now >= expires) return PHOTO_FMT_NO;

    if (mode == PHOTO_FMT_CORRUPT)
        return force_flag ? PHOTO_FMT_GO_FORCE : PHOTO_FMT_NO;
    if (mode == PHOTO_FMT_BLANK || mode == PHOTO_FMT_RESUME)
        return PHOTO_FMT_GO;
    return PHOTO_FMT_NO;
}

bool photo_token_valid(const char *tok)
{
    if (!tok) return false;
    size_t n = strlen(tok);
    if (n < 8 || n > 64) return false;
    for (size_t i = 0; i < n; i++) {
        char c = tok[i];
        if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') ||
              (c >= 'A' && c <= 'F')))
            return false;
    }
    return true;
}

int photo_backoff_s(int fails)
{
    if (fails >= 3) return -1;
    if (fails == 2) return 300;
    if (fails == 1) return 60;
    return 0;
}

static bool hex8_ok(const char *p)
{
    for (int i = 0; i < 8; i++) {
        char c = p[i];
        if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')))
            return false;
    }
    return p[8] == 0;
}

bool photo_bad_has(const photo_bad_t *b, const char prefix[9])
{
    if (!b || !prefix || !hex8_ok(prefix)) return false;
    for (int i = 0; i < b->count; i++)
        if (memcmp(b->prefix[i], prefix, 9) == 0) return true;
    return false;
}

bool photo_bad_add(photo_bad_t *b, const char sha_hex[65])
{
    if (!b || !sha_hex) return false;
    char p[9];
    memcpy(p, sha_hex, 8);
    p[8] = 0;
    /* sha 必须是 64 位小写 hex；前缀再用 hex8_ok 兜一次。 */
    for (int i = 0; i < 64; i++) {
        char c = sha_hex[i];
        if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'))) return false;
    }
    if (!hex8_ok(p)) return false;
    if (photo_bad_has(b, p)) return true;
    if (b->count >= PHOTO_BAD_MAX) return false;
    memcpy(b->prefix[b->count], p, 9);
    b->count++;
    return true;
}

const char *photo_state_name(photo_state_t st)
{
    switch (st) {
    case PHOTO_READY:  return "ok";
    case PHOTO_UNINIT: return "uninit";
    case PHOTO_FAULT:  return "fault";
    case PHOTO_ABSENT:
    default:           return "absent";
    }
}

int photo_status_json(const photo_status_t *s, char *out, size_t cap)
{
    if (!s || !out || cap == 0) return -1;
    const char *state = s->state ? s->state : "absent";
    const char *done = s->init_done ? s->init_done : "";
    int n = snprintf(out, cap,
                     "{\"cap\":%s,\"ready\":%s,\"plv\":%d,\"have\":%d,\"bad\":[",
                     s->cap ? "true" : "false", s->ready ? "true" : "false",
                     s->plv, s->have);
    if (n < 0 || (size_t)n >= cap) return -1;
    int used = 0;
    if (s->bad) {
        for (int i = 0; i < s->bad->count && i < PHOTO_BAD_MAX; i++) {
            int k = snprintf(out + n, cap - (size_t)n, "%s\"%s\"",
                             used ? "," : "", s->bad->prefix[i]);
            if (k < 0 || (size_t)k >= cap - (size_t)n) return -1;
            n += k;
            used++;
        }
    }
    int k = snprintf(out + n, cap - (size_t)n,
                     "],\"free_kb\":%u,\"state\":\"%s\",\"init_done\":\"%s\"}",
                     (unsigned)s->free_kb, state, done);
    if (k < 0 || (size_t)k >= cap - (size_t)n) return -1;
    return n + k;
}
