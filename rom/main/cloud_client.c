/*
 * 云端客户端：HTTPS 注册/换配对码 + 一条 MQTT over TLS 长连接。
 *
 *   - 没有设备密钥：POST /api/device/register {dev}，拿到密钥和 6 位配对码，
 *     电视和 LCD 显示；密钥写 NVS。
 *   - MQTT 连上后：每 30 秒发 status 心跳；订阅 usage（用量）与 pair（配对通知）。
 *   - 收到 paired=true：切看板；paired=false：清密钥重启，回到注册出码。
 *   - 配对码 10 分钟过期：用当前待配对密钥调 /api/device/pair-code 换新码。
 *   - CONNACK 0x05（密钥被吊销/服务端不认）：清 NVS 密钥并重启，重新注册，
 *     不再永久停止重连。
 */
#include <stdio.h>
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_log.h"
#include "esp_system.h"
#include "esp_random.h"
#include "esp_timer.h"
#include "esp_http_client.h"
#include "esp_crt_bundle.h"
#include "mqtt_client.h"
#include "cJSON.h"

#include "cloud_client.h"
#include "coredump.h"
#include "lcd_ui.h"
#include "ota.h"
#include "pairband.h"
#include "provision.h"

static const char *TAG = "cloud";

/* cloud_creds.h（已 gitignore）里的地址是【默认值】，不是唯一来源：
 * 运行期优先用配网页写进 NVS 的自托管地址，没配过才回落到这里。
 * 这样一份预编译固件所有人都能用，见 docs/PRODUCT.md 4.1。
 * 本机测试可以在 cloud_creds.h 里定义 STICKBOX_INSECURE_TLS=1 跳过证书校验。 */
#include "cloud_creds.h"

#ifndef API_BASE
#error "cloud_creds.h 需要 API_BASE（例如 https://host/api）"
#endif
#ifndef MQTT_URI
#error "cloud_creds.h 需要 MQTT_URI（例如 mqtts://host:8883）"
#endif

#ifdef STICKBOX_INSECURE_TLS
#warning "STICKBOX_INSECURE_TLS is enabled: TLS certificate verification is disabled (local test only)"
#endif

#define BACKOFF_MIN_MS  2000
#define BACKOFF_MAX_MS 60000
#define HEARTBEAT_S       30
#define RX_MAX          1024
#define PAIR_RETRY_S      30

/* 运行期生效的服务器地址：NVS 优先，空则用编译期默认值。
 * cloud_client_start() 里填好，之后只读。 */
static char s_mqtt_uri[128];
static char s_api_base[128];

static esp_mqtt_client_handle_t s_mqtt;
static char s_devid[24];
/* 设备密钥 {key_id}.{secret}。按不透明字符串处理，不打进日志。 */
static char s_devkey[80];
static char s_topic_status[64], s_topic_usage[64], s_topic_pair[64];
static char s_topic_frame[64];

static bool     s_paired;
static volatile bool s_connected;
static volatile bool s_got_usage;   /* 收到过用量 = 密钥已转正 */
static int s_sub_usage_id = -1, s_sub_frame_id = -1;   /* 用来识别 SUBACK */
static char     s_key_id[24];       /* 设备令牌里第一个 '.' 之前的部分 */
static char     s_pair_code[8];
static int64_t  s_pair_expire_us;   /* 本机 uptime 截止时间 */
static bool     s_auth_failed;
static uint32_t s_backoff_ms = BACKOFF_MIN_MS;

/* MQTT DATA 可能分片。topic 只在第一片（offset==0）里有效，
 * 后续分片靠 s_rx_kind 记住「正在收什么」。 */
enum { RX_NONE, RX_USAGE, RX_PAIR };
static int  s_rx_kind;
static int  s_rx_len;
static char s_rx_buf[RX_MAX + 1];

static void subscribe_paired_topics(void);

bool cloud_paired(void) { return s_paired; }

/* MQTT 是否已连上。OTA 健康检查拿它当「这版固件能用」的判据之一——
 * 判据必须是设备自己能决定的，不能依赖电视开没开、用户配没配对。 */
bool cloud_connected(void) { return s_connected; }

const char *cloud_api_base(void) { return s_api_base; }

const char *cloud_devkey(void) { return s_devkey; }

/* 把 API 路径拼到运行期的 base 上。以前是 API_BASE "/x" 的字面量拼接，
 * 地址进 NVS 之后不能再在编译期拼。 */
static void api_url(char *buf, size_t cap, const char *path)
{
    snprintf(buf, cap, "%s%s", s_api_base, path);
}

// set_devkey 记住令牌并取出 key_id：配对通知要按 key_id 过滤，
// 否则上一轮 retain 的通知会被下一轮注册误读。
static void set_devkey(const char *tok)
{
    strlcpy(s_devkey, tok, sizeof s_devkey);
    s_key_id[0] = 0;
    const char *dot = strchr(tok, '.');
    if (dot) {
        size_t n = (size_t)(dot - tok);
        if (n >= sizeof s_key_id) n = sizeof s_key_id - 1;
        memcpy(s_key_id, tok, n);
        s_key_id[n] = 0;
    }
}

/* ── HTTPS ── */

static int http_post(const char *url, const char *body, const char *bearer,
                     char *resp, size_t cap)
{
    esp_http_client_config_t cfg = {
        .url = url,
        .method = HTTP_METHOD_POST,
        .timeout_ms = 15000,
        .buffer_size = 1024,
#ifndef STICKBOX_INSECURE_TLS
        .crt_bundle_attach = esp_crt_bundle_attach,
#endif
    };
    esp_http_client_handle_t h = esp_http_client_init(&cfg);
    if (!h) return -1;

    resp[0] = 0;
    esp_http_client_set_header(h, "Content-Type", "application/json");
    if (bearer && bearer[0]) {
        char auth[96];
        snprintf(auth, sizeof auth, "Bearer %s", bearer);
        esp_http_client_set_header(h, "Authorization", auth);
    }
    int len = (int)strlen(body);
    int status = -1;
    if (esp_http_client_open(h, len) == ESP_OK) {
        esp_http_client_write(h, body, len);
        esp_http_client_fetch_headers(h);
        status = esp_http_client_get_status_code(h);
        int n = esp_http_client_read(h, resp, cap - 1);
        if (n > 0) resp[n] = 0;
        else resp[0] = 0;
    }
    esp_http_client_cleanup(h);
    return status;
}

static int64_t json_i64(const cJSON *root, const char *key, int64_t dflt)
{
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(root, key);
    return cJSON_IsNumber(v) ? (int64_t)v->valuedouble : dflt;
}

// set_pair 用服务端给的 now/expires_at 算出本机 uptime 截止点。
static void set_pair(const char *code, int64_t expires, int64_t now)
{
    if (!code || strlen(code) != 6) return;
    int64_t ttl = expires > now ? expires - now : 600;
    strlcpy(s_pair_code, code, sizeof s_pair_code);
    s_pair_expire_us = esp_timer_get_time() + ttl * 1000000LL;
    s_paired = false;
    lcd_ui_pair(code, false);
    pairband_show(code);
    ESP_LOGI(TAG, "配对码 %s，%lld 秒后过期", code, (long long)ttl);
}

static bool device_register(void)
{
    char body[64], resp[512], url[160];
    snprintf(body, sizeof body, "{\"dev\":\"%s\"}", s_devid);
    api_url(url, sizeof url, "/device/register");
    int st = http_post(url, body, NULL, resp, sizeof resp);
    if (st != 200) {
        ESP_LOGW(TAG, "注册失败 HTTP %d：%s", st, resp);
        return false;
    }
    cJSON *root = cJSON_Parse(resp);
    if (!root) {
        ESP_LOGW(TAG, "注册响应不是 JSON：%s", resp);
        return false;
    }
    const cJSON *tok = cJSON_GetObjectItemCaseSensitive(root, "token");
    const cJSON *code = cJSON_GetObjectItemCaseSensitive(root, "code");
    int64_t expires = json_i64(root, "expires_at", 0);
    int64_t now = json_i64(root, "now", 0);
    bool ok = cJSON_IsString(tok) && tok->valuestring[0] &&
              cJSON_IsString(code) && strlen(code->valuestring) == 6;
    if (ok) {
        set_devkey(tok->valuestring);
        prov_save_devkey(s_devkey);
        set_pair(code->valuestring, expires, now);
        ESP_LOGI(TAG, "设备已注册，等待配对");
    }
    cJSON_Delete(root);
    return ok;
}

static void pair_code_refresh(void)
{
    char resp[512], url[160];
    api_url(url, sizeof url, "/device/pair-code");
    int st = http_post(url, "{}", s_devkey, resp, sizeof resp);
    if (st == 401 || st == 403) {
        ESP_LOGE(TAG, "设备密钥被拒，清掉后重新注册");
        prov_erase_devkey();
        s_devkey[0] = 0;
        lcd_ui_cloud("密钥被拒，重新注册");
        vTaskDelay(pdMS_TO_TICKS(1500));
        esp_restart();
        return;
    }
    if (st != 200) {
        ESP_LOGW(TAG, "换配对码失败 HTTP %d：%s", st, resp);
        s_pair_expire_us = esp_timer_get_time() + PAIR_RETRY_S * 1000000LL;
        return;
    }
    cJSON *root = cJSON_Parse(resp);
    if (!root) return;
    const cJSON *code = cJSON_GetObjectItemCaseSensitive(root, "code");
    int64_t expires = json_i64(root, "expires_at", 0);
    int64_t now = json_i64(root, "now", 0);
    if (cJSON_IsString(code))
        set_pair(code->valuestring, expires, now);
    cJSON_Delete(root);
}

/* ── 用量 / 配对通知 ── */

static void read_window(const cJSON *root, const char *name, usage_window_t *w)
{
    const cJSON *o = cJSON_GetObjectItemCaseSensitive(root, name);
    if (!cJSON_IsObject(o)) return;
    const cJSON *pct = cJSON_GetObjectItemCaseSensitive(o, "used_percentage");
    const cJSON *rst = cJSON_GetObjectItemCaseSensitive(o, "resets_at");
    if (!cJSON_IsNumber(pct) || !cJSON_IsNumber(rst)) return;
    w->present = true;
    w->used_pct = (float)pct->valuedouble;
    w->resets_at = (int64_t)rst->valuedouble;
}

static void handle_usage(const char *json, int len)
{
    cJSON *root = cJSON_ParseWithLength(json, len);
    if (!root) {
        ESP_LOGW(TAG, "用量 payload 不是合法 JSON（%d 字节）", len);
        return;
    }
    usage_report_t u = { 0 };
    u.now = json_i64(root, "now", 0);
    u.ts = json_i64(root, "ts", 0);
    read_window(root, "five_hour", &u.five_hour);
    read_window(root, "seven_day", &u.seven_day);
    cJSON_Delete(root);

    if (!u.now) {
        ESP_LOGW(TAG, "用量 payload 缺 now，丢弃");
        return;
    }
    s_got_usage = true;
    lcd_ui_usage(&u);
    ESP_LOGI(TAG, "收到用量：5h=%.1f%% 7d=%.1f%% ts=%lld 堆=%u",
             u.five_hour.used_pct, u.seven_day.used_pct,
             (long long)u.ts, (unsigned)esp_get_free_heap_size());
}

static void handle_pair(const char *json, int len)
{
    cJSON *root = cJSON_ParseWithLength(json, len);
    if (!root) return;
    const cJSON *p = cJSON_GetObjectItemCaseSensitive(root, "paired");
    bool paired = cJSON_IsBool(p) && cJSON_IsTrue(p);

    /* 只认发给当前密钥的通知。
     *   paired=true  带 key_id
     *   paired=false 带被吊销的 key_ids 列表
     * 重新注册后 key_id 变了，旧 retain 消息会被忽略。 */
    bool mine = false;
    if (paired) {
        const cJSON *kid = cJSON_GetObjectItemCaseSensitive(root, "key_id");
        mine = !cJSON_IsString(kid) || strcmp(kid->valuestring, s_key_id) == 0;
    } else {
        const cJSON *kids = cJSON_GetObjectItemCaseSensitive(root, "key_ids");
        if (!cJSON_IsArray(kids)) {
            mine = true;   /* 兼容不带 key_ids 的旧服务端 */
        } else {
            const cJSON *it = NULL;
            cJSON_ArrayForEach(it, kids) {
                if (cJSON_IsString(it) && strcmp(it->valuestring, s_key_id) == 0) {
                    mine = true;
                    break;
                }
            }
        }
    }
    cJSON_Delete(root);

    if (!mine) {
        ESP_LOGI(TAG, "忽略不属于当前密钥 %s 的配对通知", s_key_id);
        return;
    }

    if (paired) {
        s_paired = true;
        s_pair_code[0] = 0;
        lcd_ui_pair("", true);
        lcd_ui_cloud("已配对");
        pairband_clear();
        subscribe_paired_topics();
        ESP_LOGI(TAG, "配对成功，切到看板");
    } else {
        ESP_LOGW(TAG, "设备已解绑，清密钥重新注册");
        prov_erase_devkey();
        s_devkey[0] = 0;
        lcd_ui_pair("", false);
        lcd_ui_cloud("已解绑");
        vTaskDelay(pdMS_TO_TICKS(1500));
        esp_restart();
    }
}

static void heartbeat_task(void *arg)
{
    (void)arg;
    char p[256];
    while (1) {
        if (s_mqtt) {
            /* fw 和 reset：服务端不解 ELF，没有版本号的 coredump 还原不了符号；
             * reset 让「设备为什么又上线了」有答案（T10）。 */
            int n = snprintf(p, sizeof p,
                "{\"dev\":\"%s\",\"state\":\"online\",\"heap\":%u,\"heap_min\":%u,"
                "\"uptime\":%lld,\"fw\":\"%s\",\"reset\":\"%s\"}",
                s_devid, (unsigned)esp_get_free_heap_size(),
                (unsigned)esp_get_minimum_free_heap_size(),
                (long long)(esp_timer_get_time() / 1000000),
                ota_version(), coredump_reset_reason());
            esp_mqtt_client_publish(s_mqtt, s_topic_status, p, n, 1, 0);
            ESP_LOGI(TAG, "心跳 heap=%u min=%u",
                     (unsigned)esp_get_free_heap_size(),
                     (unsigned)esp_get_minimum_free_heap_size());
        }
        vTaskDelay(pdMS_TO_TICKS(HEARTBEAT_S * 1000));
    }
}

static void pair_code_task(void *arg)
{
    (void)arg;
    while (1) {
        vTaskDelay(pdMS_TO_TICKS(5000));
        if (s_paired || s_got_usage) {
            /* 配对完成后不再需要换码；8KB 栈不能在看板运行期常驻。
             * s_got_usage 是第二道保险：能收到用量就说明密钥已经转正，
             * 不必非等那条可能被 key_id 过滤掉的 paired=true。 */
            ESP_LOGI(TAG, "已配对，换码任务退出");
            vTaskDelete(NULL);
            return;
        }
        if (!s_devkey[0] || !s_pair_expire_us) continue;
        if (esp_timer_get_time() > s_pair_expire_us) {
            ESP_LOGI(TAG, "配对码过期，换新码");
            pair_code_refresh();
        }
    }
}

/* 配对之后才有权订阅的主题。
 *
 * 必须在「配对成功」这个时刻补订一次，不能只在连接建立时订：密钥是原地
 * 转正的，配对前后是同一条 MQTT 连接，不会重连。只在连接时订的话，设备
 * 配对成功后永远收不到用量，电视就停在配对页——而两端都不报错。
 *
 * 重复订阅是无害的，所以不必记状态。 */
/* 配对之后才有权订阅的主题：用量与帧版本通知。
 *
 * **有密钥就订，不等 paired=true。** mochi 的 retained 消息只在内存里，
 * 服务端一重启那条 paired=true 就没了；只认它的话，设备会卡在「已配对却
 * 收不到任何数据」，而两端都不报错。所以只要本地有密钥、MQTT 也连上了，
 * 就直接订——真是 pending，服务端会用 SUBACK 0x80 拒掉，那时再等配对通知
 * 补订即可。
 *
 * 重复订阅无害，所以不必记状态。 */
static void subscribe_paired_topics(void)
{
    if (!s_mqtt) return;
    s_sub_usage_id = esp_mqtt_client_subscribe(s_mqtt, s_topic_usage, 1);
    s_sub_frame_id = esp_mqtt_client_subscribe(s_mqtt, s_topic_frame, 1);
    ESP_LOGI(TAG, "已请求订阅用量与帧通知（msg_id %d/%d）",
             s_sub_usage_id, s_sub_frame_id);
}

static void on_mqtt(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    esp_mqtt_event_handle_t e = data;
    switch ((esp_mqtt_event_id_t)id) {
    case MQTT_EVENT_CONNECTED:
        s_connected = true;
        ESP_LOGI(TAG, "已连接云端 %s", s_mqtt_uri);
        lcd_ui_cloud("已连接");
        s_backoff_ms = BACKOFF_MIN_MS;
        esp_mqtt_set_config(s_mqtt, &(esp_mqtt_client_config_t){
            .network.reconnect_timeout_ms = s_backoff_ms });
        /* pair 永远要订：待配对设备靠它收「已配对」。
         * usage 只有已配对才订——服务端的 ACL 不允许待配对密钥读 usage，
         * 订了也是被拒。配对成功时再补订，见 subscribe_paired_topics()。 */
        esp_mqtt_client_subscribe(s_mqtt, s_topic_pair, 1);
        /* 手里有密钥就直接订用量与帧通知，不等 paired=true——理由见
         * subscribe_paired_topics() 的注释。被拒了会走 SUBACK 分支。 */
        if (s_devkey[0]) subscribe_paired_topics();
        break;

    case MQTT_EVENT_SUBSCRIBED:
        /* SUBACK 里 0x80 = 拒绝。走到这儿说明密钥还是 pending（配对没完成，
         * 或者服务端认为没完成）。不是错误，等 paired=true 到了再补订。 */
        if (e->data_len > 0 && (uint8_t)e->data[0] == 0x80) {
            if (e->msg_id == s_sub_usage_id || e->msg_id == s_sub_frame_id)
                ESP_LOGW(TAG, "订阅被拒（msg_id %d），密钥尚未转正，等配对通知", e->msg_id);
        }
        break;

    case MQTT_EVENT_DISCONNECTED:
        s_connected = false;
        if (s_auth_failed) break;
        s_backoff_ms = s_backoff_ms * 2;
        if (s_backoff_ms > BACKOFF_MAX_MS) s_backoff_ms = BACKOFF_MAX_MS;
        {
            int jitter = (int)(esp_random() % (s_backoff_ms / 5 * 2 + 1))
                       - (int)(s_backoff_ms / 5);
            int next = (int)s_backoff_ms + jitter;
            if (next < 1000) next = 1000;
            ESP_LOGW(TAG, "与云端断开，%d ms 后重连", next);
            esp_mqtt_set_config(s_mqtt, &(esp_mqtt_client_config_t){
                .network.reconnect_timeout_ms = next });
        }
        lcd_ui_cloud("重连中");
        break;

    case MQTT_EVENT_DATA: {
        /* 第一片才有 topic；后续分片的 topic 是 NULL，不能在这里判断。 */
        if (e->current_data_offset == 0) {
            s_rx_kind = RX_NONE;
            s_rx_len = 0;
            if (e->topic && e->topic_len == (int)strlen(s_topic_usage) &&
                strncmp(e->topic, s_topic_usage, e->topic_len) == 0)
                s_rx_kind = RX_USAGE;
            else if (e->topic && e->topic_len == (int)strlen(s_topic_pair) &&
                     strncmp(e->topic, s_topic_pair, e->topic_len) == 0)
                s_rx_kind = RX_PAIR;
        }
        if (s_rx_kind == RX_NONE) break;
        if (e->total_data_len <= 0 || e->total_data_len > RX_MAX ||
            e->current_data_offset != s_rx_len ||
            s_rx_len + e->data_len > RX_MAX) {
            ESP_LOGW(TAG, "分片不连续（offset=%d len=%d total=%d），丢弃",
                     e->current_data_offset, e->data_len, e->total_data_len);
            s_rx_kind = RX_NONE;
            s_rx_len = 0;
            break;
        }
        memcpy(s_rx_buf + s_rx_len, e->data, e->data_len);
        s_rx_len += e->data_len;
        if (s_rx_len >= e->total_data_len) {
            s_rx_buf[s_rx_len] = 0;
            if (s_rx_kind == RX_USAGE) handle_usage(s_rx_buf, s_rx_len);
            else                       handle_pair(s_rx_buf, s_rx_len);
            s_rx_kind = RX_NONE;
            s_rx_len = 0;
        }
        break;
    }

    case MQTT_EVENT_ERROR:
        if (e->error_handle &&
            e->error_handle->error_type == MQTT_ERROR_TYPE_CONNECTION_REFUSED &&
            e->error_handle->connect_return_code ==
                MQTT_CONNECTION_REFUSE_NOT_AUTHORIZED) {
            if (!s_auth_failed) {
                s_auth_failed = true;
                ESP_LOGE(TAG, "云端拒绝认证（密钥被吊销或服务端不认）——"
                              "清密钥重新注册");
                lcd_ui_cloud("密钥被拒，重新注册");
                prov_erase_devkey();
                s_devkey[0] = 0;
                vTaskDelay(pdMS_TO_TICKS(1500));
                esp_restart();
            }
        } else {
            ESP_LOGE(TAG, "MQTT 错误 type=%d",
                     e->error_handle ? e->error_handle->error_type : -1);
            lcd_ui_cloud("连接错误");
        }
        break;

    default:
        break;
    }
}

void cloud_client_start(const uint8_t mac[6])
{
    snprintf(s_devid, sizeof s_devid, "%02x%02x%02x%02x%02x%02x",
             mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]);
    snprintf(s_topic_status, sizeof s_topic_status, "printer/%s/status", s_devid);
    snprintf(s_topic_usage, sizeof s_topic_usage, "printer/%s/usage", s_devid);
    snprintf(s_topic_pair, sizeof s_topic_pair, "printer/%s/pair", s_devid);
    snprintf(s_topic_frame, sizeof s_topic_frame, "printer/%s/frame", s_devid);
    ESP_LOGI(TAG, "设备 ID: %s", s_devid);

    /* 服务器地址：NVS 里的自托管值优先，哪一项为空就用编译期默认。 */
    prov_load_server(s_mqtt_uri, sizeof s_mqtt_uri, s_api_base, sizeof s_api_base);
    bool self_hosted = s_mqtt_uri[0] || s_api_base[0];
    if (!s_mqtt_uri[0]) strlcpy(s_mqtt_uri, MQTT_URI, sizeof s_mqtt_uri);
    if (!s_api_base[0]) strlcpy(s_api_base, API_BASE, sizeof s_api_base);
    ESP_LOGI(TAG, "服务器（%s）：%s / %s",
             self_hosted ? "NVS 自托管" : "编译期默认", s_mqtt_uri, s_api_base);

    char keybuf[80];
    /* 只有真的走了注册流程、手里有一个待配对的码时，才需要换码任务。
     * 从 NVS 读出密钥的那条路**根本不创建它**——见下面的理由。 */
    bool need_pair_code = false;

    if (!prov_load_devkey(keybuf, sizeof keybuf)) {
        need_pair_code = true;
        lcd_ui_cloud("正在注册");
        while (!device_register()) {
            lcd_ui_cloud("注册失败，重试中");
            vTaskDelay(pdMS_TO_TICKS(PAIR_RETRY_S * 1000));
        }
    } else {
        set_devkey(keybuf);
        ESP_LOGI(TAG, "设备密钥已加载（%d 字符）", (int)strlen(s_devkey));
        lcd_ui_pair("", false);   /* 配对状态以服务端 retain 的 pair 消息为准 */
    }

    /* 4096：T8 实测余量 556 字节，偏紧，加 1KB。 */
    xTaskCreate(heartbeat_task, "heartbeat", 4096, NULL, 3, NULL);

    /* 换配对码任务只在「这次开机真的注册了、手里有码」时才创建。
     *
     * 原来是无条件创建、只在收到 paired=true 时退出。而已配对设备开机时
     * 从 NVS 读出密钥、压根不会注册，也就没有码；服务端那条 retained 的
     * pair 消息又是针对旧 key_id 的，会被 key_id 过滤掉——于是这个任务
     * 永远等不到 paired=true，8KB 栈就这么一直占着，还会周期性开 TLS
     * 去换一个根本不存在的码，每次约 40KB 峰值。
     * 上板实测这是稳态少掉几十 KB 的大头。 */
    if (need_pair_code) {
        /* 换配对码要跑一次 TLS 握手，4096 太紧；和注册用同一个 8192。 */
        xTaskCreate(pair_code_task, "pair_code", 8192, NULL, 3, NULL);
    } else {
        ESP_LOGI(TAG, "已有设备密钥，不创建换码任务");
    }

    char lwt[96];
    int lwtn = snprintf(lwt, sizeof lwt,
                        "{\"dev\":\"%s\",\"state\":\"offline\"}", s_devid);

    const esp_mqtt_client_config_t cfg = {
        .broker.address.uri = s_mqtt_uri,
#ifndef STICKBOX_INSECURE_TLS
        .broker.verification.crt_bundle_attach = esp_crt_bundle_attach,
#endif
        .credentials.username = s_devid,
        .credentials.authentication.password = s_devkey,
        .credentials.client_id = s_devid,
        .session.keepalive = 60,
        .session.disable_clean_session = false,
        .session.last_will = {
            .topic = s_topic_status, .msg = lwt, .msg_len = lwtn,
            .qos = 1, .retain = 1,
        },
        .network.reconnect_timeout_ms = BACKOFF_MIN_MS,
        .network.disable_auto_reconnect = false,
    };
    s_mqtt = esp_mqtt_client_init(&cfg);
    esp_mqtt_client_register_event(s_mqtt, ESP_EVENT_ANY_ID, on_mqtt, NULL);
    esp_mqtt_client_start(s_mqtt);
}
