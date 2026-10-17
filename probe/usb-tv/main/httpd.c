/*
 * 控制面板。你会站在电视前面而不是坐在电脑前，所以这是一个手机能直接用的
 * 网页，不是只能 curl 的接口。
 *
 * 所有动作都用 GET —— 不 RESTful，但这样手机浏览器地址栏和 curl 都能直接打，
 * 对一个一次性诊断工具来说这个取舍是对的。
 *
 * 弹出/拔插要睡 1~2 秒，不能在 httpd 的任务里睡（会把服务器堵住），
 * 所以丢给一个 worker 任务做，网页立刻得到响应。
 */

#include "httpd.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "esp_heap_caps.h"
#include "esp_http_server.h"
#include "esp_log.h"
#include "esp_system.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/task.h"

#include "usbdev.h"
#include "vdisk.h"

static const char *TAG = "httpd";

/* ── worker ── */
typedef enum { CMD_EJECT, CMD_REPLUG } cmd_kind_t;
typedef struct {
    cmd_kind_t kind;
    uint32_t   ms;
} cmd_t;
static QueueHandle_t s_q;

static void worker(void *arg)
{
    (void)arg;
    cmd_t c;
    while (1) {
        if (xQueueReceive(s_q, &c, portMAX_DELAY) != pdTRUE) continue;
        if (c.kind == CMD_EJECT) usbdev_eject_cycle(c.ms);
        else                     usbdev_replug(c.ms);
    }
}

/* ── 取查询参数 ── */
static int qint(httpd_req_t *r, const char *key, int dflt)
{
    char q[64], v[16];
    if (httpd_req_get_url_query_str(r, q, sizeof q) != ESP_OK) return dflt;
    if (httpd_query_key_value(q, key, v, sizeof v) != ESP_OK) return dflt;
    return atoi(v);
}

static esp_err_t txt(httpd_req_t *r, const char *s)
{
    httpd_resp_set_type(r, "text/plain; charset=utf-8");
    httpd_resp_set_hdr(r, "Cache-Control", "no-store");
    return httpd_resp_sendstr(r, s);
}

/* ── 页面 ── */
static const char PAGE[] =
"<!doctype html><meta charset=utf-8>"
"<meta name=viewport content='width=device-width,initial-scale=1'>"
"<title>VDISK PROBE</title><style>"
"body{background:#111;color:#eee;font:16px/1.5 system-ui,sans-serif;margin:0;padding:16px}"
"h1{font-size:18px;margin:0 0 4px}p.s{color:#999;font-size:13px;margin:0 0 16px}"
"button{display:block;width:100%;padding:16px;margin:8px 0;font-size:17px;border:0;"
"border-radius:10px;background:#2a4d7a;color:#fff}button.w{background:#7a3b2a}"
"button.g{background:#2a5c33}.row{display:flex;gap:8px}.row button{flex:1}"
"pre{background:#000;padding:12px;border-radius:8px;overflow:auto;font-size:12px;"
"white-space:pre-wrap;color:#9f9}label{font-size:14px;color:#bbb}"
"</style><h1>ESP32 虚拟 U 盘探针</h1>"
"<p class=s>顺序：① 归零 → ② 选一种换片方式 → ③ 看 fat/dir 有没有再涨</p>"
"<div class=row>"
"<button class=g onclick=\"go('/reset')\">① 计数归零</button>"
"<button onclick=\"upd()\">刷新</button></div>"
"<label>换到哪张（1~3）</label><div class=row>"
"<button onclick=\"arm(0)\">1</button><button onclick=\"arm(1)\">2</button>"
"<button onclick=\"arm(2)\">3</button></div>"
"<button class=w onclick=\"go('/hot?i='+sel)\">② A 热换（不弹出，预期失败）</button>"
"<button onclick=\"go('/eject?ms=1500')\">② B 介质弹出 + UNIT ATTENTION</button>"
"<button onclick=\"go('/replug?ms=1500')\">② C USB 软拔插</button>"
"<button onclick=\"go('/serial?vary=1')\">可变序列号：开（伪装成新 U 盘）</button>"
"<pre id=o>…</pre>"
"<script>var sel=0;"
"function arm(i){sel=i;go('/arm?i='+i)}"
"function go(u){fetch(u).then(r=>r.text()).then(t=>{o.textContent=t;setTimeout(upd,2200)})}"
"function upd(){fetch('/status').then(r=>r.text()).then(t=>o.textContent=t)}"
"upd();setInterval(upd,3000);</script>";

static esp_err_t h_root(httpd_req_t *r)
{
    httpd_resp_set_type(r, "text/html; charset=utf-8");
    return httpd_resp_send(r, PAGE, HTTPD_RESP_USE_STRLEN);
}

static esp_err_t h_status(httpd_req_t *r)
{
    vd_stats_t s;
    vd_stats_get(&s);
    char b[640];
    snprintf(b, sizeof b,
             "挂载      %s\n"
             "当前/待生效 slide%d / slide%d  (共 %d 张)\n"
             "文件大小  %u 字节\n"
             "序列号    %s  (可变:%s)\n"
             "空闲堆    %u  最小历史 %u\n"
             "\n"
             "── 读访问计数（归零后观察）──\n"
             "read10 调用  %u\n"
             "扇区合计     %u\n"
             "  mbr   %u\n"
             "  bpb   %u\n"
             "  fat   %u   <= 重扫的标志\n"
             "  dir   %u   <= 重扫的标志\n"
             "  data  %u\n"
             "  hole  %u\n"
             "最后 LBA     %u\n",
             usbdev_mounted() ? "是" : "否", vd_slide_current() + 1, vd_slide_pending() + 1,
             vd_slide_count(), (unsigned)vd_slide_size(), usbdev_serial(),
             usbdev_serial_varying() ? "开" : "关",
             (unsigned)esp_get_free_heap_size(),
             (unsigned)heap_caps_get_minimum_free_size(MALLOC_CAP_DEFAULT),
             (unsigned)s.calls, (unsigned)s.sectors, (unsigned)s.n_mbr, (unsigned)s.n_bpb,
             (unsigned)s.n_fat, (unsigned)s.n_dir, (unsigned)s.n_data, (unsigned)s.n_hole,
             (unsigned)s.last_lba);
    return txt(r, b);
}

static esp_err_t h_trace(httpd_req_t *r)
{
    static char b[2048];
    vd_trace(b, sizeof b);
    return txt(r, b[0] ? b : "（还没有读访问）\n");
}

static esp_err_t h_arm(httpd_req_t *r)
{
    vd_slide_arm(qint(r, "i", 0));
    return txt(r, "已设为待生效。现在选一种换片方式。\n");
}

static esp_err_t h_hot(httpd_req_t *r)
{
    usbdev_swap_hot(qint(r, "i", 0));
    return txt(r, "已热换（未弹出）。电视大概率还显示旧图——这正是要确认的。\n");
}

static esp_err_t h_eject(httpd_req_t *r)
{
    cmd_t c = { CMD_EJECT, (uint32_t)qint(r, "ms", 1500) };
    xQueueSend(s_q, &c, 0);
    return txt(r, "介质弹出中…装回后会上报一次 UNIT ATTENTION。\n");
}

static esp_err_t h_replug(httpd_req_t *r)
{
    cmd_t c = { CMD_REPLUG, (uint32_t)qint(r, "ms", 1500) };
    xQueueSend(s_q, &c, 0);
    return txt(r, "USB 软拔插中…\n");
}

static esp_err_t h_serial(httpd_req_t *r)
{
    usbdev_set_serial_varying(qint(r, "vary", 1) != 0);
    return txt(r, "已设置。下次软拔插生效。\n");
}

static esp_err_t h_reset(httpd_req_t *r)
{
    vd_stats_reset();
    return txt(r, "计数已归零。\n");
}

void probe_httpd_start(void)
{
    s_q = xQueueCreate(4, sizeof(cmd_t));
    xTaskCreate(worker, "probe_w", 3072, NULL, 4, NULL);

    httpd_config_t cfg = HTTPD_DEFAULT_CONFIG();
    cfg.max_uri_handlers = 10;
    cfg.lru_purge_enable = true;
    httpd_handle_t h = NULL;
    if (httpd_start(&h, &cfg) != ESP_OK) {
        ESP_LOGE(TAG, "httpd 起不来");
        return;
    }
    static const httpd_uri_t u[] = {
        { "/",       HTTP_GET, h_root,   NULL },
        { "/status", HTTP_GET, h_status, NULL },
        { "/trace",  HTTP_GET, h_trace,  NULL },
        { "/arm",    HTTP_GET, h_arm,    NULL },
        { "/hot",    HTTP_GET, h_hot,    NULL },
        { "/eject",  HTTP_GET, h_eject,  NULL },
        { "/replug", HTTP_GET, h_replug, NULL },
        { "/serial", HTTP_GET, h_serial, NULL },
        { "/reset",  HTTP_GET, h_reset,  NULL },
    };
    for (size_t i = 0; i < sizeof u / sizeof u[0]; i++) httpd_register_uri_handler(h, &u[i]);
    ESP_LOGI(TAG, "控制面板已启动（端口 80）");
}
