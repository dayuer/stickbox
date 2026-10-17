/* 控制面板：手机浏览器打开 http://<板子IP>/ —— 换页、重推、看状态。全是 GET。 */
#include "ctrl.h"

#include <stdio.h>
#include <stdlib.h>

#include "esp_heap_caps.h"
#include "esp_http_server.h"
#include "esp_timer.h"
#include "stream.h"

static const dlna_tv_t *s_tv;
static const volatile bool *s_found;
static volatile bool *s_repush;

static int qint(httpd_req_t *r, const char *key, int def)
{
    char q[32], v[8];
    if (httpd_req_get_url_query_str(r, q, sizeof q) == ESP_OK &&
        httpd_query_key_value(q, key, v, sizeof v) == ESP_OK)
        return atoi(v);
    return def;
}

static esp_err_t h_status(httpd_req_t *r)
{
    char b[512];
    int64_t idle = stream_last_active_us() ? (esp_timer_get_time() - stream_last_active_us()) / 1000000 : -1;
    snprintf(b, sizeof b,
             "电视      %s\n"
             "控制地址  %s\n"
             "拉流连接  %d\n"
             "当前帧    %d / %d\n"
             "已发帧    %lu   （最后一帧 %lld 秒前）\n"
             "空闲堆    %u   最小历史 %u\n",
             *s_found ? s_tv->name : "（未找到）", *s_found ? s_tv->control : "-",
             stream_clients(), stream_current() + 1, stream_frame_count(),
             (unsigned long)stream_frames_sent(), (long long)idle,
             (unsigned)esp_get_free_heap_size(), (unsigned)esp_get_minimum_free_heap_size());
    httpd_resp_set_type(r, "text/plain; charset=utf-8");
    return httpd_resp_sendstr(r, b);
}

static esp_err_t h_set(httpd_req_t *r)
{
    stream_set(qint(r, "i", stream_current() + 1));
    return h_status(r);
}

static esp_err_t h_push(httpd_req_t *r)
{
    *s_repush = true;
    httpd_resp_set_type(r, "text/plain; charset=utf-8");
    return httpd_resp_sendstr(r, "已请求重新发现并推送\n");
}

static esp_err_t h_index(httpd_req_t *r)
{
    static const char page[] =
        "<!doctype html><meta name=viewport content='width=device-width'>"
        "<meta charset=utf-8><title>DLNA 推流探针</title>"
        "<style>body{font:16px sans-serif;margin:16px}button{display:block;width:100%;"
        "padding:14px;margin:8px 0;font-size:17px}pre{background:#eee;padding:8px}</style>"
        "<h3>DLNA 推流探针</h3>"
        "<button onclick=\"go('/set?i=0')\">第 1 页</button>"
        "<button onclick=\"go('/set?i=1')\">第 2 页</button>"
        "<button onclick=\"go('/set?i=2')\">第 3 页</button>"
        "<button onclick=\"go('/set')\">下一页</button>"
        "<button onclick=\"go('/push')\">重新发现并推送</button>"
        "<pre id=s></pre>"
        "<script>function go(u){fetch(u).then(r=>r.text()).then(t=>s.textContent=t)}"
        "go('/status');setInterval(()=>go('/status'),3000)</script>";
    httpd_resp_set_type(r, "text/html");
    return httpd_resp_send(r, page, sizeof page - 1);
}

void ctrl_start(const dlna_tv_t *tv, const volatile bool *found, volatile bool *repush_req)
{
    s_tv = tv; s_found = found; s_repush = repush_req;
    httpd_config_t c = HTTPD_DEFAULT_CONFIG();
    httpd_handle_t h;
    if (httpd_start(&h, &c) != ESP_OK) return;
    const httpd_uri_t u[] = {
        { .uri = "/", .method = HTTP_GET, .handler = h_index },
        { .uri = "/status", .method = HTTP_GET, .handler = h_status },
        { .uri = "/set", .method = HTTP_GET, .handler = h_set },
        { .uri = "/push", .method = HTTP_GET, .handler = h_push },
    };
    for (size_t i = 0; i < sizeof u / sizeof u[0]; i++) httpd_register_uri_handler(h, &u[i]);
}
