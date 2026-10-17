/* 局域网接收 Claude 用量：POST /api/usage
 *
 * 发送方是电脑上的定时任务（tools/claude-usage-push.sh，launchd 每 10 分钟一次）。
 * 请求体：
 *   {"now":1760680000,
 *    "five_hour":{"used_percentage":23.5,"resets_at":1760690000},
 *    "seven_day":{"used_percentage":41.2,"resets_at":1761200000}}
 * model / cost_usd 可选。缺席的窗口不更新，沿用上一次的值。
 *
 * 只开 2 个连接、小栈：这颗芯片上并发连接吃内存的教训见 main.c 注释。
 * 不做鉴权——只在局域网，最坏后果是屏上数字被人改了。
 */
#include <string.h>
#include "esp_http_server.h"
#include "esp_log.h"
#include "cJSON.h"
#include "lcd_ui.h"
#include "usage_http.h"

static const char *TAG = "usage_http";

#define MAX_BODY 1024

static void read_window(const cJSON *root, const char *name, usage_window_t *w)
{
    const cJSON *o = cJSON_GetObjectItemCaseSensitive(root, name);
    const cJSON *pct = cJSON_GetObjectItemCaseSensitive(o, "used_percentage");
    const cJSON *rst = cJSON_GetObjectItemCaseSensitive(o, "resets_at");
    if (!cJSON_IsNumber(pct) || !cJSON_IsNumber(rst)) return;
    w->present   = true;
    w->used_pct  = (float)pct->valuedouble;
    w->resets_at = (int64_t)rst->valuedouble;
}

static esp_err_t on_usage(httpd_req_t *req)
{
    if (req->content_len <= 0 || req->content_len > MAX_BODY)
        return httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "bad length");

    char buf[MAX_BODY + 1];
    int got = 0;
    while (got < req->content_len) {
        int n = httpd_req_recv(req, buf + got, req->content_len - got);
        if (n == HTTPD_SOCK_ERR_TIMEOUT) continue;
        if (n <= 0) return ESP_FAIL;
        got += n;
    }
    buf[got] = 0;

    cJSON *root = cJSON_Parse(buf);
    if (!root) return httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "bad json");

    usage_report_t u = { 0 };
    const cJSON *now = cJSON_GetObjectItemCaseSensitive(root, "now");
    if (cJSON_IsNumber(now)) u.now = (int64_t)now->valuedouble;
    read_window(root, "five_hour", &u.five_hour);
    read_window(root, "seven_day", &u.seven_day);
    const cJSON *cost = cJSON_GetObjectItemCaseSensitive(root, "cost_usd");
    if (cJSON_IsNumber(cost)) { u.has_cost = true; u.cost_usd = (float)cost->valuedouble; }
    const cJSON *model = cJSON_GetObjectItemCaseSensitive(root, "model");
    if (cJSON_IsString(model)) strlcpy(u.model, model->valuestring, sizeof u.model);
    cJSON_Delete(root);

    if (!u.now) return httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "missing now");

    lcd_ui_usage(&u);
    httpd_resp_set_status(req, "204 No Content");
    return httpd_resp_send(req, NULL, 0);
}

void usage_http_start(void)
{
    httpd_config_t cfg = HTTPD_DEFAULT_CONFIG();
    cfg.max_open_sockets = 2;
    cfg.max_uri_handlers = 1;
    cfg.stack_size       = 6144;    /* buf 占 1K，cJSON 解析再要一点 */
    cfg.lru_purge_enable = true;    /* 连接满了踢最老的，别让卡住的客户端占坑 */

    httpd_handle_t srv;
    if (httpd_start(&srv, &cfg) != ESP_OK) {
        ESP_LOGE(TAG, "httpd_start failed");
        return;
    }
    const httpd_uri_t uri = { .uri = "/api/usage", .method = HTTP_POST, .handler = on_usage };
    httpd_register_uri_handler(srv, &uri);
    ESP_LOGI(TAG, "listening on :80/api/usage");
}
