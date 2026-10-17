/*
 * 云端客户端：一条向外的 MQTT over TLS 长连接。
 *
 * 打印退役后这条连接只剩两件事：
 *   - 每 30 秒发一条 status 心跳（服务端据此判在线）
 *   - 订阅 printer/{dev}/usage，把服务端推来的用量交给屏幕/T4 看板
 *
 * 用量 payload（server/go/internal/usage.DevicePayload）：
 *   {"now":...,"ts":...,"five_hour":{...},"seven_day":{...}}
 * now 是服务端现场时间，ts 是服务端收到 agent 数据的时间。
 */
#include <stdio.h>
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_log.h"
#include "esp_system.h"
#include "esp_random.h"
#include "esp_timer.h"
#include "mqtt_client.h"
#include "esp_crt_bundle.h"
#include "cJSON.h"

#include "cloud_client.h"
#include "lcd_ui.h"
#include "provision.h"

static const char *TAG = "cloud";

/* 云端地址放在 cloud_creds.h（已 gitignore），
 * 照着 cloud_creds.h.example 复制一份填自己的。 */
#include "cloud_creds.h"

#define BACKOFF_MIN_MS  2000      /* 重连退避起步 */
#define BACKOFF_MAX_MS 60000
#define HEARTBEAT_S       30
#define USAGE_MAX       1024      /* 服务端 payload 只有几百字节 */

static esp_mqtt_client_handle_t s_mqtt;
static char s_devid[24];
/* 设备密钥 {key_id}.{secret}，45 字符。按不透明字符串处理——
 * 不解析、不截断、**不打进日志**。 */
static char s_devkey[80];
static char s_topic_status[64], s_topic_usage[64];

static bool     s_auth_failed;        /* CONNACK 0x05：停止重连，别刷服务端 */
static uint32_t s_backoff_ms = BACKOFF_MIN_MS;

/* usage 可能分片到达，按 offset 拼进同一个缓冲。 */
static char s_usage_buf[USAGE_MAX + 1];
static int  s_usage_len;

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
    const cJSON *now = cJSON_GetObjectItemCaseSensitive(root, "now");
    const cJSON *ts = cJSON_GetObjectItemCaseSensitive(root, "ts");
    if (cJSON_IsNumber(now)) u.now = (int64_t)now->valuedouble;
    if (cJSON_IsNumber(ts)) u.ts = (int64_t)ts->valuedouble;
    read_window(root, "five_hour", &u.five_hour);
    read_window(root, "seven_day", &u.seven_day);
    cJSON_Delete(root);

    if (!u.now) {
        ESP_LOGW(TAG, "用量 payload 缺 now，丢弃");
        return;
    }
    lcd_ui_usage(&u);
    ESP_LOGI(TAG, "收到用量：5h=%.1f%% 7d=%.1f%% ts=%lld 堆=%u",
             u.five_hour.used_pct, u.seven_day.used_pct,
             (long long)u.ts, (unsigned)esp_get_free_heap_size());
}

static void publish_heartbeat(void)
{
    if (!s_mqtt) return;
    char p[192];
    int n = snprintf(p, sizeof p,
        "{\"dev\":\"%s\",\"state\":\"online\",\"heap\":%u,\"heap_min\":%u,"
        "\"uptime\":%lld}",
        s_devid, (unsigned)esp_get_free_heap_size(),
        (unsigned)esp_get_minimum_free_heap_size(),
        (long long)(esp_timer_get_time() / 1000000));
    esp_mqtt_client_publish(s_mqtt, s_topic_status, p, n, 1, 0);
    ESP_LOGI(TAG, "心跳 heap=%u min=%u",
             (unsigned)esp_get_free_heap_size(),
             (unsigned)esp_get_minimum_free_heap_size());
}

static void heartbeat_task(void *arg)
{
    (void)arg;
    while (1) {
        publish_heartbeat();
        vTaskDelay(pdMS_TO_TICKS(HEARTBEAT_S * 1000));
    }
}

/* 密钥被拒之后隔多久再试一次。
 *
 * 不能永远不试：密钥问题多半在服务端修好，而设备这头不会知道。
 * 也不能按常规退避去试：错的密钥每次都要服务端跑一遍 argon2，全网设备
 * 一起撞会把服务端压垮。半小时一次是折中。 */
#define AUTH_RETRY_US (30ULL * 60 * 1000 * 1000)

static esp_timer_handle_t s_auth_retry;

/* 认证被拒后真正把客户端停掉。不能在 MQTT 任务里直接 stop。 */
static void auth_stop_cb(void *arg)
{
    (void)arg;
    if (s_mqtt) {
        esp_mqtt_client_stop(s_mqtt);
        ESP_LOGW(TAG, "MQTT 客户端已停止，%d 分钟后再试一次",
                 (int)(AUTH_RETRY_US / 60000000ULL));
    }
}

static void auth_retry_cb(void *arg)
{
    (void)arg;
    if (!s_mqtt) return;
    ESP_LOGI(TAG, "重试云端认证");
    lcd_ui_cloud("云端重试中");
    s_auth_failed = false;
    s_backoff_ms = BACKOFF_MIN_MS;
    esp_mqtt_client_start(s_mqtt);
}

static void on_mqtt(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    esp_mqtt_event_handle_t e = data;
    switch ((esp_mqtt_event_id_t)id) {
    case MQTT_EVENT_CONNECTED:
        ESP_LOGI(TAG, "已连接云端 %s", MQTT_URI);
        lcd_ui_cloud("已连接");
        s_backoff_ms = BACKOFF_MIN_MS;
        esp_mqtt_set_config(s_mqtt, &(esp_mqtt_client_config_t){
            .network.reconnect_timeout_ms = s_backoff_ms });
        esp_mqtt_client_subscribe(s_mqtt, s_topic_usage, 1);
        publish_heartbeat();       /* 不等第一拍 30 秒，连上就报一次在线 */
        break;

    case MQTT_EVENT_DISCONNECTED:
        if (s_auth_failed) break;
        /* 指数退避 + ±20% 抖动，避免全网设备同时重连。 */
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
        if (!e->topic || e->topic_len != (int)strlen(s_topic_usage) ||
            strncmp(e->topic, s_topic_usage, e->topic_len) != 0)
            break;

        if (e->total_data_len <= 0 || e->total_data_len > USAGE_MAX) {
            ESP_LOGW(TAG, "用量 payload 过长（%d 字节），丢弃", e->total_data_len);
            s_usage_len = 0;
            break;
        }
        if (e->current_data_offset == 0) s_usage_len = 0;
        if (e->current_data_offset != s_usage_len ||
            s_usage_len + e->data_len > USAGE_MAX) {
            ESP_LOGW(TAG, "用量分片不连续（offset=%d len=%d），丢弃本次",
                     e->current_data_offset, e->data_len);
            s_usage_len = 0;
            break;
        }
        memcpy(s_usage_buf + s_usage_len, e->data, e->data_len);
        s_usage_len += e->data_len;
        if (s_usage_len >= e->total_data_len) {
            s_usage_buf[s_usage_len] = 0;
            handle_usage(s_usage_buf, s_usage_len);
            s_usage_len = 0;
        }
        break;
    }

    case MQTT_EVENT_ERROR:
        /* CONNACK 0x05 = Not authorized：密钥错了，重试一万次也是错的。 */
        if (e->error_handle &&
            e->error_handle->error_type == MQTT_ERROR_TYPE_CONNECTION_REFUSED &&
            e->error_handle->connect_return_code ==
                MQTT_CONNECTION_REFUSE_NOT_AUTHORIZED) {
            if (!s_auth_failed) {
                s_auth_failed = true;
                ESP_LOGE(TAG, "云端拒绝认证（密钥错误或已吊销）——停止重连。"
                              "开机按住 MENU 键清配置后重新配网");
                lcd_ui_cloud("密钥被拒");
                /* 不能在 MQTT 回调里直接 stop，用一次性定时器从定时器任务里停。 */
                esp_timer_handle_t t;
                const esp_timer_create_args_t a = {
                    .callback = auth_stop_cb, .name = "mqtt_stop",
                };
                if (esp_timer_create(&a, &t) == ESP_OK)
                    esp_timer_start_once(t, 100 * 1000);

                if (!s_auth_retry) {
                    const esp_timer_create_args_t ra = {
                        .callback = auth_retry_cb, .name = "mqtt_auth_retry",
                    };
                    esp_timer_create(&ra, &s_auth_retry);
                }
                if (s_auth_retry) {
                    esp_timer_stop(s_auth_retry);
                    esp_timer_start_once(s_auth_retry, AUTH_RETRY_US);
                }
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
    ESP_LOGI(TAG, "设备 ID: %s", s_devid);

    /* 没有设备密钥就不要连——服务端会以 CONNACK 0x05 拒绝，
     * 而「被拒 → 重连」是个死循环。停在这里等用户配网。 */
    if (!prov_load_devkey(s_devkey, sizeof s_devkey)) {
        ESP_LOGE(TAG, "未配置设备密钥，不连接云端。"
                      "开机按住 MENU 键清配置，在配网页里填入密钥");
        lcd_ui_cloud("缺设备密钥");
        return;
    }
    ESP_LOGI(TAG, "设备密钥已加载（%d 字符）", (int)strlen(s_devkey));

    xTaskCreate(heartbeat_task, "heartbeat", 3072, NULL, 3, NULL);

    char lwt[96];
    int lwtn = snprintf(lwt, sizeof lwt,
                        "{\"dev\":\"%s\",\"state\":\"offline\"}", s_devid);

    const esp_mqtt_client_config_t cfg = {
        .broker.address.uri = MQTT_URI,
        .broker.verification.crt_bundle_attach = esp_crt_bundle_attach,
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
