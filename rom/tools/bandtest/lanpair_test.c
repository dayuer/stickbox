/* #28 局域网配对：来源判断 / 响应 / 限速 / 发现 的 host 用例。 */
#include <stdio.h>
#include <string.h>
#include "lanpair_core.h"

static int fails;
#define CHECK(c) do { if (!(c)) { printf("FAIL %s:%d %s\n", __FILE__, __LINE__, #c); fails++; } } while (0)
#define IP(a,b,c,d) (((uint32_t)(a)<<24)|((uint32_t)(b)<<16)|((uint32_t)(c)<<8)|(uint32_t)(d))

int main(void)
{
    uint32_t m24 = IP(255,255,255,0), me = IP(192,168,3,50);
    /* 来源：私有 + 同网段 */
    CHECK(lanpair_src_allowed(IP(192,168,3,237), me, m24));
    CHECK(!lanpair_src_allowed(IP(192,168,4,237), me, m24));   /* 私有但异网段 */
    CHECK(!lanpair_src_allowed(IP(8,8,8,8), me, m24));
    CHECK(!lanpair_src_allowed(IP(127,0,0,1), me, m24));
    CHECK(!lanpair_src_allowed(IP(169,254,1,1), me, m24));
    CHECK(!lanpair_src_allowed(IP(192,168,3,237), me, 0));     /* 接口未就绪 */
    CHECK(!lanpair_src_allowed(IP(192,168,3,237), IP(8,8,8,8), m24)); /* 本机非私有 */
    CHECK(lanpair_src_allowed(IP(10,1,2,3), IP(10,200,0,9), IP(255,0,0,0)));
    CHECK(lanpair_is_private(IP(172,16,0,1)) && lanpair_is_private(IP(172,31,255,255)));
    CHECK(!lanpair_is_private(IP(172,15,255,255)) && !lanpair_is_private(IP(172,32,0,0)));
    CHECK(!lanpair_is_private(IP(192,167,0,1)) && !lanpair_is_private(IP(11,0,0,1)));
    /* /16 网络：同 /16 不同 /24 应放行 */
    CHECK(lanpair_src_allowed(IP(172,20,9,9), IP(172,20,1,1), IP(255,255,0,0)));

    char b[400];
    /* 待配对 -> 200 + no-store + 正文 */
    size_t n = lanpair_http_pair(b, sizeof b, true, "aabbccddeeff", "123456", 542);
    CHECK(n > 0 && strncmp(b, "HTTP/1.0 200 OK", 15) == 0);
    CHECK(strstr(b, "Cache-Control: no-store"));
    CHECK(strstr(b, "\r\n\r\n{\"dev\":\"aabbccddeeff\",\"code\":\"123456\",\"expires_in\":542}"));
    /* 已配对 / 无码 / 过期 / 坏码 -> 404，且不含码 */
    n = lanpair_http_pair(b, sizeof b, false, "aabbccddeeff", "123456", 542);
    CHECK(n > 0 && strncmp(b, "HTTP/1.0 404", 12) == 0 && !strstr(b, "123456"));
    CHECK(strstr(b, "Cache-Control: no-store"));
    n = lanpair_http_pair(b, sizeof b, true, "aabbccddeeff", "", 542);
    CHECK(strncmp(b, "HTTP/1.0 404", 12) == 0);
    n = lanpair_http_pair(b, sizeof b, true, "aabbccddeeff", "123456", 0);
    CHECK(strncmp(b, "HTTP/1.0 404", 12) == 0 && !strstr(b, "123456"));
    n = lanpair_http_pair(b, sizeof b, true, "aabbccddeeff", "12\"456", 5);
    CHECK(strncmp(b, "HTTP/1.0 404", 12) == 0);
    n = lanpair_http_pair(b, 20, true, "aabbccddeeff", "123456", 5);
    CHECK(n == 0);
    n = lanpair_http_deny(b, sizeof b, 403);
    CHECK(n > 0 && strncmp(b, "HTTP/1.0 403", 12) == 0 && strstr(b, "no-store"));
    n = lanpair_http_deny(b, sizeof b, 429);
    CHECK(n > 0 && strncmp(b, "HTTP/1.0 429", 12) == 0);

    /* 限速：同源第 11 次拒，换源不受影响，窗口过后恢复 */
    lanpair_rl_t rl; memset(&rl, 0, sizeof rl);
    uint32_t a = IP(192,168,3,10), c = IP(192,168,3,11);
    for (int i = 0; i < 10; i++) CHECK(lanpair_rl_allow(&rl, a, 1000 + i));
    CHECK(!lanpair_rl_allow(&rl, a, 2000));
    CHECK(lanpair_rl_allow(&rl, c, 2000));
    CHECK(!lanpair_rl_allow(&rl, a, 1000 + 59999));
    CHECK(lanpair_rl_allow(&rl, a, 1000 + 60000));
    /* 表满：新来源踢走窗口最早的一项，不崩 */
    lanpair_rl_t r2; memset(&r2, 0, sizeof r2);
    for (uint32_t i = 0; i < 6; i++) CHECK(lanpair_rl_allow(&r2, IP(10,0,0,i+1), 100 + i));
    /* now_ms 回绕 */
    lanpair_rl_t r3; memset(&r3, 0, sizeof r3);
    for (int i = 0; i < 10; i++) CHECK(lanpair_rl_allow(&r3, a, 0xffffff00u + i));
    CHECK(!lanpair_rl_allow(&r3, a, 0x10u));
    CHECK(lanpair_rl_allow(&r3, a, 0xffffff00u + 60000u));

    /* UDP 发现 */
    CHECK(lanpair_disc_is_query((const uint8_t *)"SBPAIR?1", 8));
    CHECK(lanpair_disc_is_query((const uint8_t *)"SBPAIR?1\r\n", 10));
    CHECK(!lanpair_disc_is_query((const uint8_t *)"SBPAIR?2", 8));
    CHECK(!lanpair_disc_is_query((const uint8_t *)"SBPAIR?1x", 9));
    CHECK(!lanpair_disc_is_query((const uint8_t *)"SBPAIR", 6));
    CHECK(!lanpair_disc_is_query(NULL, 0));
    n = lanpair_disc_reply(b, sizeof b, true, "aabbccddeeff", "1.2.3", 8080);
    CHECK(n > 0 && strcmp(b, "{\"v\":1,\"dev\":\"aabbccddeeff\",\"pending\":true,\"fw\":\"1.2.3\",\"port\":8080}") == 0);
    CHECK(!strstr(b, "code"));
    CHECK(lanpair_disc_reply(b, sizeof b, false, "aabbccddeeff", "1.2.3", 8080) == 0);
    n = lanpair_disc_reply(b, sizeof b, true, "aabbccddeeff", "1\"}x", 8080);
    CHECK(strstr(b, "\"fw\":\"1__x\""));

    if (fails) { printf("%d 项失败\n", fails); return 1; }
    printf("lanpair: 全部通过\n");
    return 0;
}
