/*
 * 局域网配对（#28）的 IDF 侧接线。判断与拼装全在 lanpair_core.c（host 已测）。
 *
 * 绝不在此打印配对码：netlog 会把所有日志广播到局域网。
 *
 * 内存：无任务、无 malloc 常驻。常驻只有两张限速表（BSS）和一个 udp_pcb
 * （lwIP memp 池）。UDP 应答的 pbuf 在回调里申请并立即释放。
 */
#include "lanpair.h"

#include <string.h>

#include "esp_log.h"
#include "esp_netif.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "lwip/netif.h"
#include "lwip/pbuf.h"
#include "lwip/sockets.h"
#include "lwip/tcpip.h"
#include "lwip/udp.h"

#include "cloud_client.h"
#include "lanpair_core.h"
#include "ota.h"
#include "stream.h"

static const char *TAG = "lanpair";

/* HTTP 与 UDP 各一张表：agent 反复发现不该把 /pair 的额度吃光。 */
static lanpair_rl_t s_rl_http, s_rl_udp;
static portMUX_TYPE s_rl_mux = portMUX_INITIALIZER_UNLOCKED;

static uint32_t now_ms(void) { return (uint32_t)(esp_timer_get_time() / 1000); }

/* 来源通过「私有 + 同网段」后再扣限速额度。返回 0 放行，否则是 HTTP 状态。 */
static int admit(lanpair_rl_t *rl, uint32_t src, uint32_t lip, uint32_t lmask)
{
    if (!lanpair_src_allowed(src, lip, lmask)) return 403;
    taskENTER_CRITICAL(&s_rl_mux);
    bool ok = lanpair_rl_allow(rl, src, now_ms());
    taskEXIT_CRITICAL(&s_rl_mux);
    return ok ? 0 : 429;
}

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

void lanpair_http(int fd)
{
    char buf[256];
    size_t n;
    struct sockaddr_in peer;
    socklen_t pl = sizeof peer;
    uint32_t lip, lmask;
    int st = 403;
    if (getpeername(fd, (struct sockaddr *)&peer, &pl) == 0 &&
        peer.sin_family == AF_INET && local_net(&lip, &lmask))
        st = admit(&s_rl_http, ntohl(peer.sin_addr.s_addr), lip, lmask);
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

/* ---- UDP 发现：lwIP raw 回调，跑在 tcpip 线程，不得阻塞 ---- */

static void on_udp(void *arg, struct udp_pcb *pcb, struct pbuf *p,
                   const ip_addr_t *addr, u16_t port)
{
    (void)arg;
    struct netif *in = ip_current_input_netif();
    /* 已配对 / 没码：什么都不回，也不扣额度。 */
    if (p && p->len == p->tot_len && in && IP_IS_V4(addr) &&
        cloud_pair_pending() && !cloud_paired() &&
        lanpair_disc_is_query((const uint8_t *)p->payload, p->len)) {
        uint32_t src = ntohl(ip4_addr_get_u32(ip_2_ip4(addr)));
        uint32_t lip = ntohl(ip4_addr_get_u32(netif_ip4_addr(in)));
        uint32_t lmask = ntohl(ip4_addr_get_u32(netif_ip4_netmask(in)));
        if (admit(&s_rl_udp, src, lip, lmask) == 0) {
            char r[112];
            size_t n = lanpair_disc_reply(r, sizeof r, true, cloud_devid(),
                                          ota_version(), STREAM_PORT);
            if (n) {
                struct pbuf *q = pbuf_alloc(PBUF_TRANSPORT, (u16_t)n, PBUF_RAM);
                if (q) {
                    memcpy(q->payload, r, n);
                    udp_sendto(pcb, q, addr, port);
                    pbuf_free(q);
                }
            }
        }
    }
    if (p) pbuf_free(p);
}

static void udp_bind_cb(void *arg)
{
    (void)arg;
    struct udp_pcb *pcb = udp_new();
    if (!pcb) { ESP_LOGW(TAG, "udp_new 失败"); return; }
    if (udp_bind(pcb, IP4_ADDR_ANY, LANPAIR_UDP_PORT) != ERR_OK) {
        ESP_LOGW(TAG, "udp_bind :%d 失败", LANPAIR_UDP_PORT);
        udp_remove(pcb);
        return;
    }
    udp_recv(pcb, on_udp, NULL);
}

void lanpair_udp_start(void)
{
    if (tcpip_callback(udp_bind_cb, NULL) != ERR_OK)
        ESP_LOGW(TAG, "tcpip_callback 失败，局域网发现不可用");
}
