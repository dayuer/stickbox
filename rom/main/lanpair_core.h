#pragma once
/*
 * 局域网配对发现（#28）的纯逻辑：来源判断、限速、响应拼装。
 * 不依赖 IDF/lwIP，host 可测（rom/tools/bandtest/lanpair_test.c）。
 *
 * 地址一律用主机字节序的 uint32（a.b.c.d -> a<<24|b<<16|c<<8|d）。
 * 码绝不写日志：本文件没有任何日志调用，调用方也不要打印响应体。
 */
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#define LANPAIR_UDP_PORT   5141
#define LANPAIR_HTTP_LIMIT 10        /* 每个来源每窗口最多次数 */
#define LANPAIR_WINDOW_MS  60000u
#define LANPAIR_RL_SLOTS   4

/* 私有地址段：10/8、172.16/12、192.168/16。 */
bool lanpair_is_private(uint32_t ip);

/* 来源是否允许：必须是私有地址，且与本机同一网段（ip & mask 相同）。
 * mask==0（接口未就绪）一律拒绝。 */
bool lanpair_src_allowed(uint32_t src, uint32_t local_ip, uint32_t local_mask);

/* 固定窗口限速表，每个来源每 LANPAIR_WINDOW_MS 最多 LANPAIR_HTTP_LIMIT 次。
 * 4 项，常驻 BSS。满了先踢已过期的，再踢窗口起点最早的。
 * 非线程安全，调用方自己串行化。 */
typedef struct {
    struct { uint32_t ip; uint32_t t0; uint16_t n; bool used; } e[LANPAIR_RL_SLOTS];
} lanpair_rl_t;

bool lanpair_rl_allow(lanpair_rl_t *rl, uint32_t src, uint32_t now_ms);

/* 6 位数字码才算有效，防止把奇怪内容拼进 JSON。 */
bool lanpair_code_valid(const char *code);

/* GET /pair 的完整 HTTP 响应（含头）。pending && 码有效 && expires_in>0 -> 200
 * {"dev","code","expires_in"}；否则 404 无正文。均带 Cache-Control: no-store。
 * 返回写入长度（不含结尾 0）；cap 不够返回 0。 */
size_t lanpair_http_pair(char *buf, size_t cap, bool pending, const char *dev,
                         const char *code, int expires_in);
/* 给被拒的来源/超限的请求：status 为 403 或 429，无正文。 */
size_t lanpair_http_deny(char *buf, size_t cap, int status);

/* UDP 发现请求：载荷必须是 "SBPAIR?1"（允许末尾 \r\n）。 */
bool lanpair_disc_is_query(const uint8_t *data, size_t len);
/* 发现应答 {"v":1,"dev","pending":true,"fw","port"}。未待配对返回 0（不应答）。
 * fw 里非 [0-9A-Za-z._-] 的字符会被替换为 '_'。 */
size_t lanpair_disc_reply(char *buf, size_t cap, bool pending, const char *dev,
                          const char *fw, int port);
