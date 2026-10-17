/*
 * DLNA / UPnP 控制点的最小子集：SSDP 发现 + AVTransport 的 Stop / SetAVTransportURI / Play。
 *
 * 实测（三星 UA60DU7000，Tizen）：
 *   - 正在显示内容时直接 SetAVTransportURI 回 701 Transition not available，先 Stop 才行
 *   - 图片 / 流在 SetAVTransportURI 时就开始拉，随后的 Play 可能回 701，不算失败
 *   - 直接断流电视会报「网络中断」；要收尾先发 Stop
 */
#include "dlna.h"

#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "esp_http_client.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "lwip/inet.h"
#include "lwip/sockets.h"

static const char *TAG = "dlna";

#define AVT "urn:schemas-upnp-org:service:AVTransport:1"

/* ── 小工具 ── */
static const char *find_ci(const char *hay, const char *needle)
{
    size_t n = strlen(needle);
    for (; *hay; hay++)
        if (strncasecmp(hay, needle, n) == 0) return hay;
    return NULL;
}

/* 取 <tag>…</tag> 之间的文本，从 from 开始找 */
static bool xml_text(const char *from, const char *tag, char *out, size_t cap)
{
    char open[48];
    snprintf(open, sizeof open, "<%s>", tag);
    const char *a = strstr(from, open);
    if (!a) return false;
    a += strlen(open);
    const char *b = strchr(a, '<');
    if (!b) return false;
    size_t n = b - a < (int)cap - 1 ? (size_t)(b - a) : cap - 1;
    memcpy(out, a, n);
    out[n] = 0;
    return true;
}

/* 简单 HTTP 请求：body 读进堆上缓冲，调用者 free */
static int http_req(const char *url, const char *method, const char *soap_action,
                    const char *body, char **resp)
{
    esp_http_client_config_t c = { .url = url, .timeout_ms = 5000 };
    esp_http_client_handle_t h = esp_http_client_init(&c);
    if (!h) return -1;
    if (!strcmp(method, "POST")) {
        esp_http_client_set_method(h, HTTP_METHOD_POST);
        esp_http_client_set_header(h, "Content-Type", "text/xml; charset=\"utf-8\"");
        esp_http_client_set_header(h, "SOAPACTION", soap_action);
    }
    int blen = body ? strlen(body) : 0;
    int status = -1;
    *resp = NULL;
    if (esp_http_client_open(h, blen) == ESP_OK) {
        if (blen) esp_http_client_write(h, body, blen);
        esp_http_client_fetch_headers(h);
        status = esp_http_client_get_status_code(h);
        const int cap = 24 * 1024;
        char *buf = malloc(cap);
        int got = 0, r;
        while (buf && got < cap - 1 && (r = esp_http_client_read(h, buf + got, cap - 1 - got)) > 0)
            got += r;
        if (buf) buf[got] = 0;
        *resp = buf;
    }
    esp_http_client_cleanup(h);
    return status;
}

/* ── 发现 ── */
static bool ssdp_location(char *loc, size_t cap, char *ip, size_t ipcap)
{
    int s = socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    if (s < 0) return false;
    struct timeval tv = { .tv_sec = 1 };
    setsockopt(s, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
    uint8_t ttl = 2;
    setsockopt(s, IPPROTO_IP, IP_MULTICAST_TTL, &ttl, sizeof ttl);

    static const char msearch[] =
        "M-SEARCH * HTTP/1.1\r\n"
        "HOST: 239.255.255.250:1900\r\n"
        "MAN: \"ssdp:discover\"\r\n"
        "MX: 2\r\n"
        "ST: " AVT "\r\n\r\n";
    struct sockaddr_in dst = { .sin_family = AF_INET, .sin_port = htons(1900) };
    inet_aton("239.255.255.250", &dst.sin_addr);

    bool found = false;
    char buf[1024];
    for (int round = 0; round < 3 && !found; round++) {
        sendto(s, msearch, sizeof msearch - 1, 0, (struct sockaddr *)&dst, sizeof dst);
        for (int k = 0; k < 4 && !found; k++) {
            struct sockaddr_in src;
            socklen_t sl = sizeof src;
            int n = recvfrom(s, buf, sizeof buf - 1, 0, (struct sockaddr *)&src, &sl);
            if (n <= 0) continue;
            buf[n] = 0;
            const char *l = find_ci(buf, "\r\nLOCATION:");
            if (!l) continue;
            l += 11;
            while (*l == ' ') l++;
            size_t i = 0;
            while (l[i] && l[i] != '\r' && i < cap - 1) { loc[i] = l[i]; i++; }
            loc[i] = 0;
            inet_ntoa_r(src.sin_addr, ip, ipcap);
            found = true;
        }
    }
    close(s);
    return found;
}

bool dlna_find_tv(const char *manual_ip, dlna_tv_t *out)
{
    memset(out, 0, sizeof *out);
    if (manual_ip && *manual_ip) {
        strlcpy(out->ip, manual_ip, sizeof out->ip);
        strlcpy(out->name, "（手动指定）", sizeof out->name);
        snprintf(out->control, sizeof out->control,
                 "http://%s:9197/upnp/control/AVTransport1", manual_ip);
        return true;
    }

    char loc[160];
    if (!ssdp_location(loc, sizeof loc, out->ip, sizeof out->ip)) {
        ESP_LOGW(TAG, "SSDP 没找到 AVTransport 渲染器");
        return false;
    }
    ESP_LOGI(TAG, "SSDP: %s → %s", out->ip, loc);

    char *desc;
    int st = http_req(loc, "GET", NULL, NULL, &desc);
    if (st != 200 || !desc) {
        ESP_LOGW(TAG, "取设备描述失败 %d", st);
        free(desc);
        return false;
    }
    xml_text(desc, "friendlyName", out->name, sizeof out->name);

    const char *svc = strstr(desc, AVT "</serviceType>");
    char ctl[96] = "";
    if (!svc || !xml_text(svc, "controlURL", ctl, sizeof ctl)) {
        ESP_LOGW(TAG, "描述里没有 AVTransport controlURL");
        free(desc);
        return false;
    }
    free(desc);

    /* controlURL 相对 LOCATION 的 scheme://host:port */
    const char *hp = strstr(loc, "://");
    const char *path = hp ? strchr(hp + 3, '/') : NULL;
    int base = path ? (int)(path - loc) : (int)strlen(loc);
    snprintf(out->control, sizeof out->control, "%.*s%s%s", base, loc, ctl[0] == '/' ? "" : "/", ctl);
    ESP_LOGW(TAG, "找到电视「%s」 %s", out->name, out->control);
    return true;
}

/* ── SOAP ── */
static int soap(const dlna_tv_t *tv, const char *action, const char *args, bool *is701)
{
    size_t cap = strlen(args) + 512;
    char *env = malloc(cap);
    if (!env) return -1;
    snprintf(env, cap,
             "<?xml version=\"1.0\" encoding=\"utf-8\"?>"
             "<s:Envelope xmlns:s=\"http://schemas.xmlsoap.org/soap/envelope/\" "
             "s:encodingStyle=\"http://schemas.xmlsoap.org/soap/encoding/\"><s:Body>"
             "<u:%s xmlns:u=\"" AVT "\">%s</u:%s></s:Body></s:Envelope>",
             action, args, action);
    char sa[96];
    snprintf(sa, sizeof sa, "\"" AVT "#%s\"", action);
    char *resp;
    int st = http_req(tv->control, "POST", sa, env, &resp);
    free(env);
    if (is701) *is701 = resp && strstr(resp, "<errorCode>701<");
    if (st != 200) {
        char code[8] = "?";
        if (resp) xml_text(resp, "errorCode", code, sizeof code);
        ESP_LOGW(TAG, "%s → HTTP %d UPnP %s", action, st, code);
    }
    free(resp);
    return st;
}

/* XML 转义一遍（DIDL 嵌进 SOAP 参数里，需要整体再转义） */
static void xml_escape(const char *in, char *out, size_t cap)
{
    size_t o = 0;
    for (; *in && o + 7 < cap; in++) {
        const char *r = NULL;
        switch (*in) {
        case '&': r = "&amp;"; break;
        case '<': r = "&lt;"; break;
        case '>': r = "&gt;"; break;
        case '"': r = "&quot;"; break;
        }
        if (r) { size_t n = strlen(r); memcpy(out + o, r, n); o += n; }
        else out[o++] = *in;
    }
    out[o] = 0;
}

bool dlna_stop(const dlna_tv_t *tv)
{
    bool e701;
    int st = soap(tv, "Stop", "<InstanceID>0</InstanceID>", &e701);
    return st == 200 || e701;
}

bool dlna_push(const dlna_tv_t *tv, const char *url, const char *mime, const char *title)
{
    /* 这三步各自耗时都要能看见。「首推后约 4 秒断流」有两个说得通的原因，
     * 只有把这里的时刻和 stream 的「电视开始拉流」对上才能分清：
     *   - Play 打断加载：电视在 SetURI 之后就来连 :8080，而我们紧接着又发
     *     Play，它在 TRANSITIONING 里被要求 Play，于是重起一次加载；
     *   - 电视自己就要断：第一条连接只是探流，断开是它的正常流程。
     * 判据：「电视开始拉流」出现在下面 Play 那行之前还是之后。 */
    const int64_t t_begin = esp_timer_get_time();
    dlna_stop(tv);

    const bool video = strncmp(mime, "video/", 6) == 0;
    char didl[640];
    snprintf(didl, sizeof didl,
             "<DIDL-Lite xmlns=\"urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/\" "
             "xmlns:dc=\"http://purl.org/dc/elements/1.1/\" "
             "xmlns:upnp=\"urn:schemas-upnp-org:metadata-1-0/upnp/\">"
             "<item id=\"0\" parentID=\"-1\" restricted=\"1\"><dc:title>%s</dc:title>"
             "<upnp:class>%s</upnp:class>"
             "<res protocolInfo=\"http-get:*:%s:*\">%s</res></item></DIDL-Lite>",
             title, video ? "object.item.videoItem" : "object.item.imageItem.photo", mime, url);
    char *esc = malloc(1600), *args = malloc(2000);
    if (!esc || !args) { free(esc); free(args); return false; }
    xml_escape(didl, esc, 1600);
    snprintf(args, 2000, "<InstanceID>0</InstanceID><CurrentURI>%s</CurrentURI>"
                         "<CurrentURIMetaData>%s</CurrentURIMetaData>", url, esc);
    int st = soap(tv, "SetAVTransportURI", args, NULL);
    free(esc); free(args);
    if (st != 200) return false;
    const int64_t t_seturi = esp_timer_get_time();
    ESP_LOGW(TAG, "SetAVTransportURI 返回 200（Stop+SetURI 共 %lld 毫秒），接着发 Play",
             (t_seturi - t_begin) / 1000);

    bool e701;
    st = soap(tv, "Play", "<InstanceID>0</InstanceID><Speed>1</Speed>", &e701);
    ESP_LOGW(TAG, "Play 返回 %d%s，距 SetURI %lld 毫秒", st, e701 ? "（701，电视仍在加载）" : "",
             (esp_timer_get_time() - t_seturi) / 1000);
    if (st != 200 && !e701) return false;
    ESP_LOGW(TAG, "已推送到「%s」：%s", tv->name, url);
    return true;
}
