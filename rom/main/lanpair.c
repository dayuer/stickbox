/*
 * 局域网配对（#28）的 IDF 侧接线。判断与拼装全在 lanpair_core.c（host 已测）。
 *
 * 绝不在此打印配对码：netlog 会把所有日志广播到局域网。
 */
#include "lanpair.h"

#include "esp_netif.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "lwip/sockets.h"

#include "cloud_client.h"
#include "lanpair_core.h"

static lanpair_rl_t s_rl;
static portMUX_TYPE s_rl_mux = portMUX_INITIALIZER_UNLOCKED;

static uint32_t now_ms(void) { return (uint32_t)(esp_timer_get_time() / 1000); }

/* 本机 STA 的 IP 与掩码（主机字节序）。未连上返回 false。 */
static bool local_net(uint32_t *ip, uint32_t *mask)
{
    esp_netif_t *n = esp_netif_get_handle_from_ifkey("WIFI_STA_DEF");
    esp_netif_ip_info_t ii;
    if (!n || esp_netif_get_ip_info(n, &ii) != ESP_OK || ii.ip.addr == 0) return false;
    *ip = ntohl(ii.ip.addr);
    *mask = ntohl(ii.netmask.addr);
    return true;
}

/* 来源通过「私有 + 同网段」后再扣限速额度。返回 0 放行，否则是 HTTP 状态。 */
static int admit(uint32_t src)
{
    uint32_t lip, lmask;
    if (!local_net(&lip, &lmask) || !lanpair_src_allowed(src, lip, lmask)) return 403;
    taskENTER_CRITICAL(&s_rl_mux);
    bool ok = lanpair_rl_allow(&s_rl, src, now_ms());
    taskEXIT_CRITICAL(&s_rl_mux);
    return ok ? 0 : 429;
}

void lanpair_http(int fd)
{
    char buf[256];
    size_t n;
    struct sockaddr_in peer;
    socklen_t pl = sizeof peer;
    int st = 403;
    if (getpeername(fd, (struct sockaddr *)&peer, &pl) == 0 && peer.sin_family == AF_INET)
        st = admit(ntohl(peer.sin_addr.s_addr));
    if (st) {
        n = lanpair_http_deny(buf, sizeof buf, st);
    } else {
        char code[8] = "";
        int left = 0;
        bool pending = cloud_pair_peek(code, &left);
        n = lanpair_http_pair(buf, sizeof buf, pending, cloud_devid(), code, left);
        memset(code, 0, sizeof code);
    }
    if (n) send(fd, buf, n, 0);
    memset(buf, 0, sizeof buf);
}

void lanpair_udp_start(void) { }
