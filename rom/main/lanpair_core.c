#include "lanpair_core.h"
#include <stdio.h>
#include <string.h>

bool lanpair_is_private(uint32_t ip)
{
    return (ip >> 24) == 10 ||
           (ip & 0xfff00000u) == 0xac100000u ||   /* 172.16/12 */
           (ip & 0xffff0000u) == 0xc0a80000u;     /* 192.168/16 */
}

bool lanpair_src_allowed(uint32_t src, uint32_t local_ip, uint32_t local_mask)
{
    if (local_mask == 0 || !lanpair_is_private(src) || !lanpair_is_private(local_ip))
        return false;
    return (src & local_mask) == (local_ip & local_mask);
}

bool lanpair_rl_allow(lanpair_rl_t *rl, uint32_t src, uint32_t now_ms)
{
    int slot = -1, oldest = 0;
    for (int i = 0; i < LANPAIR_RL_SLOTS; i++) {
        if (rl->e[i].used && rl->e[i].ip == src) { slot = i; break; }
    }
    if (slot < 0) {
        /* 新来源：空位 > 已过期 > 窗口起点最早 */
        for (int i = 0; i < LANPAIR_RL_SLOTS; i++) {
            if (!rl->e[i].used) { slot = i; break; }
        }
        if (slot < 0) {
            for (int i = 0; i < LANPAIR_RL_SLOTS; i++) {
                if (now_ms - rl->e[i].t0 >= LANPAIR_WINDOW_MS) { slot = i; break; }
                if (now_ms - rl->e[i].t0 > now_ms - rl->e[oldest].t0) oldest = i;
            }
            if (slot < 0) slot = oldest;
        }
        rl->e[slot].used = true; rl->e[slot].ip = src;
        rl->e[slot].t0 = now_ms; rl->e[slot].n = 0;
    } else if (now_ms - rl->e[slot].t0 >= LANPAIR_WINDOW_MS) {
        rl->e[slot].t0 = now_ms; rl->e[slot].n = 0;
    }
    if (rl->e[slot].n >= LANPAIR_HTTP_LIMIT) return false;
    rl->e[slot].n++;
    return true;
}

bool lanpair_code_valid(const char *code)
{
    if (!code || strlen(code) != 6) return false;
    for (int i = 0; i < 6; i++)
        if (code[i] < '0' || code[i] > '9') return false;
    return true;
}

static size_t put_resp(char *buf, size_t cap, const char *status, const char *body)
{
    int n = snprintf(buf, cap,
        "HTTP/1.0 %s\r\nCache-Control: no-store\r\nConnection: close\r\n"
        "%sContent-Length: %u\r\n\r\n%s",
        status,
        body[0] ? "Content-Type: application/json\r\n" : "",
        (unsigned)strlen(body), body);
    return (n > 0 && (size_t)n < cap) ? (size_t)n : 0;
}

size_t lanpair_http_pair(char *buf, size_t cap, bool pending, const char *dev,
                         const char *code, int expires_in)
{
    if (!pending || !lanpair_code_valid(code) || expires_in <= 0 || !dev)
        return put_resp(buf, cap, "404 Not Found", "");
    char body[96];
    int n = snprintf(body, sizeof body,
        "{\"dev\":\"%.24s\",\"code\":\"%s\",\"expires_in\":%d}", dev, code, expires_in);
    if (n <= 0 || (size_t)n >= sizeof body) return 0;
    return put_resp(buf, cap, "200 OK", body);
}

size_t lanpair_http_deny(char *buf, size_t cap, int status)
{
    return put_resp(buf, cap, status == 429 ? "429 Too Many Requests" : "403 Forbidden", "");
}

bool lanpair_disc_is_query(const uint8_t *data, size_t len)
{
    static const char q[] = "SBPAIR?1";
    size_t ql = sizeof q - 1;
    if (!data || len < ql || memcmp(data, q, ql) != 0) return false;
    for (size_t i = ql; i < len; i++)
        if (data[i] != '\r' && data[i] != '\n') return false;
    return true;
}

size_t lanpair_disc_reply(char *buf, size_t cap, bool pending, const char *dev,
                          const char *fw, int port)
{
    if (!pending || !dev) return 0;
    char f[24];
    size_t i = 0;
    for (; fw && fw[i] && i < sizeof f - 1; i++) {
        char c = fw[i];
        bool ok = (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') ||
                  (c >= 'A' && c <= 'Z') || c == '.' || c == '_' || c == '-';
        f[i] = ok ? c : '_';
    }
    f[i] = 0;
    int n = snprintf(buf, cap,
        "{\"v\":1,\"dev\":\"%.24s\",\"pending\":true,\"fw\":\"%s\",\"port\":%d}",
        dev, f, port);
    return (n > 0 && (size_t)n < cap) ? (size_t)n : 0;
}
