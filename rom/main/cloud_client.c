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
#include "framefetch.h"
#include "framepack.h"
#include "photo_sync.h"
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
/* 连续多少次“明确拒绝”（0x04/0x05）才认定密钥失效。0x03（服务繁忙/
 * 准入限流）永远只算临时错误，绝不触发清密钥。 */
#define AUTH_FAIL_LIMIT    3

/* 运行期生效的服务器地址：NVS 优先，空则用编译期默认值。
 * cloud_client_start() 里填好，之后只读。 */
static char s_mqtt_uri[128];
static char s_api_base[128];

static esp_mqtt_client_handle_t s_mqtt;
static char s_devid[24];
/* 设备密钥 {key_id}.{secret}。按不透明字符串处理，不打进日志。 */
static char s_devkey[80];
static char s_topic_status[64], s_topic_usage[64], s_topic_pair[64];
static char s_topic_frame[64], s_topic_ack[64];

static bool     s_paired;
static volatile bool s_connected;
static volatile bool s_got_usage;   /* 收到过用量 = 密钥已转正 */
static int s_sub_usage_id = -1, s_sub_frame_id = -1;   /* 用来识别 SUBACK */
/* 这条连接上是否已经发过 usage/frame 的 SUBSCRIBE。CONNECTED 里发一次，
 * 配对成功回调里会再走同一条路径；不记状态就会每条连接订两遍，服务端把
 * 每条用量/帧通知也投两遍。被 SUBACK 拒绝时清掉，配对转正后可以重订。 */
static bool s_data_sub_req;
static char     s_key_id[24];       /* 设备令牌里第一个 '.' 之前的部分 */
static char     s_pair_code[8];
static int64_t  s_pair_expire_us;   /* 本机 uptime 截止时间 */
static bool     s_auth_failed;
static int      s_auth_fails;       /* 连续明确拒绝次数；连接成功后清零 */
static uint32_t s_backoff_ms = BACKOFF_MIN_MS;
/* 自己驱动重连。esp-mqtt 的自动重连只能靠 esp_mqtt_set_config 改间隔，
 * 而那个 API 会拿残缺结构体整体覆盖配置（broker URI / TLS 全被清空）——
 * 配对被踢后永远连不上。这里关掉自动重连，用一次性定时器按退避调
 * esp_mqtt_client_reconnect()，配置文件从头到尾不再动。 */
static esp_timer_handle_t s_reconnect_timer;
static volatile bool s_reconnect_armed;

/* MQTT DATA 可能分片。topic 只在第一片（offset==0）里有效，
 * 后续分片靠 s_rx_kind 记住「正在收什么」。 */
enum { RX_NONE, RX_USAGE, RX_PAIR, RX_FRAME };
static int  s_rx_kind;
static int  s_rx_len;
static char s_rx_buf[RX_MAX + 1];

static void subscribe_paired_topics(void);
static void schedule_cleanup_restart(void);

bool cloud_paired(void) { return s_paired; }

/* MQTT 是否已连上。OTA 健康检查拿它当「这版固件能用」的判据之一——
 * 判据必须是设备自己能决定的，不能依赖电视开没开、用户配没配对。 */
bool cloud_connected(void) { return s_connected; }

const char *cloud_api_base(void) { return s_api_base; }

const char *cloud_devkey(void) { return s_devkey; }

/* 有码就意味着还没配对，电视上挂的是配对码。配对成功时 handle_pair()
 * 会清掉它。 */
bool cloud_pair_pending(void) { return s_pair_code[0] != 0; }

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
        /* 重新注册意味着设备身份换了：flash 里上一轮的帧不能再拿给
         * 新主人看。配对成功后会重新拉。 */
        framepack_invalidate();
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
        framepack_invalidate();
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

/* ── 指令 ACK ── */

/* 云端下行的每条指令都带 cid，设备**应用完**才回 ACK，服务端收不到就重投。
 *
 * 为什么 QoS1 不够：QoS1 的 PUBACK 只说明消息投到了设备的 MQTT 栈，不说明
 * 设备真的应用了。解析失败、状态机拒绝、应用到一半掉电——这些情况服务端
 * 都以为成功，两端状态就此分叉。帧包通知尤其要命：丢一条的表现是电视永远
 * 停在旧画面，设备和服务端都不报错，没人会发现。 */
#define ACK_RING 8
#define CID_MAX  28

/* 最近已应用并 ACK 过的 cid。服务端重投时补一次 ACK 即可，不重复应用。 */
static char s_acked[ACK_RING][CID_MAX];
static int  s_acked_at;

/* pair 另记一格。配对/解绑很少见，而 usage 每 10 分钟一条，8 格环很快就
 * 把 pair 的 cid 挤掉了——服务端重投一条解绑时设备会重新执行一次。 */
static char s_acked_pair[CID_MAX];

static bool cid_seen(const char *cid)
{
    if (s_acked_pair[0] && strcmp(s_acked_pair, cid) == 0) return true;
    for (int i = 0; i < ACK_RING; i++)
        if (s_acked[i][0] && strcmp(s_acked[i], cid) == 0) return true;
    return false;
}

static void cid_remember_pair(const char *cid)
{
    strlcpy(s_acked_pair, cid, sizeof s_acked_pair);
}

static void cid_remember(const char *cid)
{
    strlcpy(s_acked[s_acked_at], cid, CID_MAX);
    s_acked_at = (s_acked_at + 1) % ACK_RING;
}

/* 取 cid。没带 cid 的是旧服务端发的：照常处理，但不回 ACK。
 *
 * 只接受 xid 的形状（20 个 [0-9a-v]）。形状不对的多半是被截断或者 JSON
 * 被拼坏了，照原样回进 ACK 的 payload 还可能把那条 JSON 也拼坏——宁可
 * 当成「没有 cid」，让服务端走超时重投。 */
static bool get_cid(const cJSON *root, char *out, size_t cap)
{
    out[0] = 0;
    const cJSON *c = cJSON_GetObjectItemCaseSensitive(root, "cid");
    if (!cJSON_IsString(c) || !c->valuestring[0]) return false;

    const char *v = c->valuestring;
    if (strlen(v) != 20 || cap < 21) {
        ESP_LOGW(TAG, "cid 形状不对，按无 cid 处理");
        return false;
    }
    for (const char *p = v; *p; p++) {
        bool okch = (*p >= '0' && *p <= '9') || (*p >= 'a' && *p <= 'v');
        if (!okch) {
            ESP_LOGW(TAG, "cid 含非法字符，按无 cid 处理");
            return false;
        }
    }
    strlcpy(out, v, cap);
    return true;
}

/* 失败也要回 ACK（ok=0）：让服务端立刻知道这条没生效，而不是干等超时
 * 再重投一遍同样会失败的东西。
 *
 * **发完就算，不重试、不看结果。** 待配对期间这条 ACK 会被服务端的 ACL
 * 丢掉（pending 密钥不许写 ack），那是正常的，不是错误：配对转正后服务端
 * 会踢掉这条连接，设备以正式身份重连，retained 的 pair 消息会带着同一个
 * cid 再来一次，那时 cid 环命中、补发一次 ACK 就接上了。
 *
 * 所以这里千万别加重试——待配对期间重试多少次都是被丢掉，纯属浪费。 */
static void send_ack(const char *cid, bool ok, const char *err)
{
    if (!cid || !cid[0] || !s_mqtt) return;
    char p[128];
    int n = (err && err[0])
        ? snprintf(p, sizeof p, "{\"cid\":\"%s\",\"ok\":%d,\"e\":\"%s\"}", cid, ok ? 1 : 0, err)
        : snprintf(p, sizeof p, "{\"cid\":\"%s\",\"ok\":%d}", cid, ok ? 1 : 0);
    /* snprintf 返回的是「本该写多少」，截断时会大于缓冲区。原样传给
     * publish 等于让它读越界。 */
    if (n < 0) return;
    if (n >= (int)sizeof p) n = (int)sizeof p - 1;
    /* 未转正时 ACL 不允许写 ack：用 QoS1 会留在 outbox，每次重连都重发，
     * 被拒后又断线，形成“重连→发 ACK→被拒→重连”的循环。pending 阶段
     * 用 QoS0，发出去就被丢，不占 outbox。转正后仍用 QoS1 保证送达。 */
    int qos = s_paired ? 1 : 0;
    esp_mqtt_client_publish(s_mqtt, s_topic_ack, p, n, qos, 0);
    ESP_LOGI(TAG, "ACK cid=%s ok=%d qos=%d%s%s", cid, ok ? 1 : 0, qos,
             (err && err[0]) ? " e=" : "", (err && err[0]) ? err : "");
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
        /* 连 cid 都取不出来，无从 ACK。服务端会超时重投。 */
        ESP_LOGW(TAG, "用量 payload 不是合法 JSON（%d 字节）", len);
        return;
    }
    char cid[CID_MAX];
    get_cid(root, cid, sizeof cid);

    /* 重投：这条之前已经应用并 ACK 过，补一次 ACK 就行，不重复应用。 */
    if (cid[0] && cid_seen(cid)) {
        cJSON_Delete(root);
        send_ack(cid, true, NULL);
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
        send_ack(cid, false, "no_now");
        return;
    }
    s_got_usage = true;
    lcd_ui_usage(&u);
    /* 先应用再记、再 ACK：顺序反了就会出现「服务端以为成功、设备其实没应用」。 */
    if (cid[0]) { cid_remember(cid); send_ack(cid, true, NULL); }
    ESP_LOGI(TAG, "收到用量：5h=%.1f%% 7d=%.1f%% ts=%lld 堆=%u",
             u.five_hour.used_pct, u.seven_day.used_pct,
             (long long)u.ts, (unsigned)esp_get_free_heap_size());
}

static void handle_pair(const char *json, int len)
{
    cJSON *root = cJSON_ParseWithLength(json, len);
    if (!root) return;
    char cid[CID_MAX];
    get_cid(root, cid, sizeof cid);
    if (cid[0] && cid_seen(cid)) {
        cJSON_Delete(root);
        send_ack(cid, true, NULL);
        return;
    }
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
            /* 解绑必须明确点名当前密钥才执行。没有 key_ids 的旧格式无法
             * 确认是发给谁的，按 stale 处理——清错密钥的代价太大。 */
            mine = false;
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
        /* 是针对本设备的 topic，但 key_id 对不上（重新注册后残留的 retain）。
         * **只记日志并回 ok=0**，绝对不能动 NVS 或重启：服务端可能把旧密钥
         * 的指令重投给新密钥，一旦在这里清密钥就会“注册→旧指令→清密钥→
         * 注册”无限循环，配对码永远配不上，还会反复擦 flash。 */
        ESP_LOGW(TAG, "配对通知不属于当前密钥 %s，忽略（stale_key）", s_key_id);
        send_ack(cid, false, "stale_key");
        return;
    }

    if (paired) {
        s_paired = true;
        s_pair_code[0] = 0;
        lcd_ui_pair("", true);
        lcd_ui_cloud("已配对");
        pairband_clear();
        subscribe_paired_topics();
        /* 刚转正，手里大概还没有帧：配对前 /device/frame 一律 403，设备只能
         * 等。这里主动叫一次，省掉最长 60 秒的兜底轮询。 */
        framefetch_request("配对完成");
        photo_sync_notify_connected();
        ESP_LOGI(TAG, "配对成功，切到看板");
        if (cid[0]) { cid_remember_pair(cid); send_ack(cid, true, NULL); }
    } else {
        ESP_LOGW(TAG, "设备已解绑，清密钥重新注册");
        prov_erase_devkey();
        s_devkey[0] = 0;
        lcd_ui_pair("", false);
        lcd_ui_cloud("已解绑");
        /* ACK 必须赶在重启之前发出去。下面那 1500ms 既是给用户看屏的时间，
         * 也是留给 MQTT 把这条 ACK 冲刷出去的时间——顺序反了服务端就永远
         * 等不到回执，会一直重投一条设备其实已经执行过的解绑。 */
        if (cid[0]) { cid_remember_pair(cid); send_ack(cid, true, NULL); }
        /* 解绑后旧主人的帧必须立刻作废，不能等重启后还被新主人看到。
         * 放到独立任务里做，先让 mqtt 任务回去把 ACK 冲刷出去。 */
        schedule_cleanup_restart();
    }
}

/* 帧版本通知：payload 是 {"ver":N}，不 retain。
 *
 * 这里只做「解出版本串」这一件事，拉帧交给 framefetch——MQTT 事件跑在 mqtt
 * 任务上，在它栈上开 TLS 下 218KB 是找崩溃。
 *
 * ver 两种写法都认：服务端现在发数字（docs/API.md 的 `{"ver":N}`），但槽头部
 * 存的是字符串，将来换成带时间戳的版本串也不用改固件。 */
static void handle_frame(const char *json, int len)
{
    cJSON *root = cJSON_ParseWithLength(json, len);
    if (!root) {
        ESP_LOGW(TAG, "帧通知不是合法 JSON（%d 字节）", len);
        return;
    }
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(root, "ver");
    char ver[24] = { 0 };
    if (cJSON_IsNumber(v))      snprintf(ver, sizeof ver, "%lld", (long long)v->valuedouble);
    else if (cJSON_IsString(v)) strlcpy(ver, v->valuestring, sizeof ver);
    /* plv 是照片清单版本；老服务端不发这个字段，照片链路就不动。 */
    const cJSON *pv = cJSON_GetObjectItemCaseSensitive(root, "plv");
    int plv = -1;
    if (cJSON_IsNumber(pv)) plv = (int)pv->valuedouble;
    cJSON_Delete(root);

    if (plv >= 0) photo_sync_notify_plv(plv);

    if (!ver[0]) {
        ESP_LOGW(TAG, "帧通知里没有 ver，忽略");
        return;
    }
    ESP_LOGI(TAG, "服务端通知新帧版本 %s", ver);
    framefetch_notify(ver);
}

static void heartbeat_task(void *arg)
{
    (void)arg;
    /* 640B：照片字段每次心跳都带，整条必须 ≤512B（docs/PHOTO.md 5.4）。 */
    char p[640];
    while (1) {
        if (s_mqtt) {
            /* fw 和 reset：服务端不解 ELF，没有版本号的 coredump 还原不了符号；
             * reset 让「设备为什么又上线了」有答案（T10）。 */
            int n = snprintf(p, sizeof p,
                "{\"dev\":\"%s\",\"state\":\"online\",\"heap\":%u,\"heap_min\":%u,"
                "\"uptime\":%lld,\"fw\":\"%s\",\"reset\":\"%s\"",
                s_devid, (unsigned)esp_get_free_heap_size(),
                (unsigned)esp_get_minimum_free_heap_size(),
                (long long)(esp_timer_get_time() / 1000000),
                ota_version(), coredump_reset_reason());
            if (n < 0) n = 0;
            if ((size_t)n < sizeof p - 2) {
                p[n++] = ',';
                int k = photo_sync_status_json(p + n, sizeof p - (size_t)n - 1);
                if (k > 0) n += k;
                else n--;
            }
            if ((size_t)n < sizeof p - 1) {
                p[n++] = '}';
                p[n] = 0;
            } else {
                p[sizeof p - 1] = 0;
                n = (int)sizeof p - 1;
            }
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
    /* 同一条连接只订一次：CONNECTED 与“配对成功”都会调到这里。 */
    if (s_data_sub_req) return;
    s_data_sub_req = true;
    s_sub_usage_id = esp_mqtt_client_subscribe(s_mqtt, s_topic_usage, 1);
    s_sub_frame_id = esp_mqtt_client_subscribe(s_mqtt, s_topic_frame, 1);
    if (s_sub_usage_id < 0 || s_sub_frame_id < 0) {
        /* 发送失败（连接刚好断开等）：清掉状态，让下次事件还能重试。 */
        s_data_sub_req = false;
    }
    ESP_LOGI(TAG, "已请求订阅用量与帧通知（msg_id %d/%d）",
             s_sub_usage_id, s_sub_frame_id);
}

/* 清槽 + 延时 + 重启不能跑在 MQTT 事件里：esp-mqtt 的 handler 就在 mqtt
 * 任务上执行，阻塞它会让刚 publish 的 ACK 永远发不出去。 */
static void cleanup_restart_task(void *arg)
{
    (void)arg;
    s_auth_failed = true;   /* 清理期间不要再发起重连 */
    /* 先让 mqtt 任务把解绑 ACK 冲刷出去，再动 flash。 */
    vTaskDelay(pdMS_TO_TICKS(1500));

    ESP_LOGW(TAG, "cleanup: 作废帧槽");
    framepack_invalidate_reboot();

    ESP_LOGW(TAG, "cleanup: 停止 MQTT client");
    if (s_mqtt) esp_mqtt_client_stop(s_mqtt);

    ESP_LOGW(TAG, "cleanup: 重启");
    esp_restart();
}

static void schedule_cleanup_restart(void)
{
    if (xTaskCreate(cleanup_restart_task, "cleanup", 3072, NULL, 3, NULL) != pdPASS) {
        esp_restart();
    }
}

static void reconnect_cb(void *arg)
{
    (void)arg;
    s_reconnect_armed = false;
    if (!s_connected && s_mqtt && !s_auth_failed) {
        esp_mqtt_client_reconnect(s_mqtt);
    }
}

/* 安排下一次重连。重复调用会重置定时器，保证同一时刻只有一个在跑。 */
static void schedule_reconnect(int delay_ms)
{
    if (!s_reconnect_timer) return;
    esp_timer_stop(s_reconnect_timer);
    s_reconnect_armed = true;
    esp_timer_start_once(s_reconnect_timer, (uint64_t)delay_ms * 1000ULL);
}

static void on_mqtt(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    esp_mqtt_event_handle_t e = data;
    switch ((esp_mqtt_event_id_t)id) {
    case MQTT_EVENT_CONNECTED:
        s_connected = true;
        s_auth_fails = 0;      /* 连上就说明当前密钥有效 */
        s_auth_failed = false;
        if (s_reconnect_timer) esp_timer_stop(s_reconnect_timer);
        s_reconnect_armed = false;
        ESP_LOGI(TAG, "已连接云端 %s", s_mqtt_uri);
        lcd_ui_cloud("已连接");
        s_backoff_ms = BACKOFF_MIN_MS;
        /* 新连接（clean session），订阅不继承，必须重新发；先把本连接的
         * 订阅状态清掉，后面配对回调才不会误判为“已经订过”。 */
        s_data_sub_req = false;
        /* pair 永远要订：待配对设备靠它收「已配对」。
         * usage 只有已配对才订——服务端的 ACL 不允许待配对密钥读 usage，
         * 订了也是被拒。配对成功时再补订，见 subscribe_paired_topics()。 */
        esp_mqtt_client_subscribe(s_mqtt, s_topic_pair, 1);
        /* 手里有密钥就直接订用量与帧通知，不等 paired=true——理由见
         * subscribe_paired_topics() 的注释。被拒了会走 SUBACK 分支。 */
        if (s_devkey[0]) {
            subscribe_paired_topics();
            /* 每次连上都对一次清单：断线期间 plv 可能已经变了，
             * ETag 会让没变化时只花一个 304。 */
            photo_sync_notify_connected();
        }
        break;

    case MQTT_EVENT_SUBSCRIBED:
        /* SUBACK 里 0x80 = 拒绝。走到这儿说明密钥还是 pending（配对没完成，
         * 或者服务端认为没完成）。不是错误，等 paired=true 到了再补订。 */
        if (e->data_len > 0 && (uint8_t)e->data[0] == 0x80) {
            if (e->msg_id == s_sub_usage_id || e->msg_id == s_sub_frame_id) {
                /* 被拒说明这条连接还是 pending；配对转正后要允许重订一次。 */
                s_data_sub_req = false;
                ESP_LOGW(TAG, "订阅被拒（msg_id %d），密钥尚未转正，等配对通知", e->msg_id);
            }
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
            schedule_reconnect(next);
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
            else if (e->topic && e->topic_len == (int)strlen(s_topic_frame) &&
                     strncmp(e->topic, s_topic_frame, e->topic_len) == 0)
                s_rx_kind = RX_FRAME;
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
            if (s_rx_kind == RX_USAGE)      handle_usage(s_rx_buf, s_rx_len);
            else if (s_rx_kind == RX_FRAME) handle_frame(s_rx_buf, s_rx_len);
            else                            handle_pair(s_rx_buf, s_rx_len);
            s_rx_kind = RX_NONE;
            s_rx_len = 0;
        }
        break;
    }

    case MQTT_EVENT_ERROR:
        if (e->error_handle &&
            e->error_handle->error_type == MQTT_ERROR_TYPE_CONNECTION_REFUSED) {
            int code = e->error_handle->connect_return_code;
            if (code == MQTT_CONNECTION_REFUSE_NOT_AUTHORIZED ||
                code == MQTT_CONNECTION_REFUSE_BAD_USERNAME) {
                /* 0x04/0x05 才可能是密钥失效；单次不说明问题（服务端重启、
                 * 旧连接未清理都可能），连续 N 次才清 NVS。 */
                s_auth_fails++;
                ESP_LOGW(TAG, "云端明确拒绝认证 0x%02x（%d/%d）",
                         code, s_auth_fails, AUTH_FAIL_LIMIT);
                if (s_auth_fails >= AUTH_FAIL_LIMIT && !s_auth_failed) {
                    s_auth_failed = true;
                    ESP_LOGE(TAG, "连续被拒，判定密钥失效——清密钥重新注册");
                    lcd_ui_cloud("密钥被拒，重新注册");
                    prov_erase_devkey();
                    s_devkey[0] = 0;
                    schedule_cleanup_restart();
                }
            } else if (code == MQTT_CONNECTION_REFUSE_SERVER_UNAVAILABLE) {
                /* 0x03：服务繁忙/连接准入限流。绝不能清密钥，退避即可。 */
                ESP_LOGW(TAG, "服务端暂时不可用（准入限流），退避后重试");
                lcd_ui_cloud("服务端繁忙，重试中");
            } else {
                ESP_LOGE(TAG, "连接被拒 0x%02x，退避重试", code);
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
    snprintf(s_topic_ack, sizeof s_topic_ack, "printer/%s/ack", s_devid);
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
        while (1) {
            if (!prov_rereg_allowed()) {
                /* 防呆：不管哪条路径出 bug 造成“注册→解绑→注册”循环，
                 * 一小时内最多写 3 次 NVS/擦 3 次帧槽。超限就停在配对页
                 * 退避，等窗口过去再自动继续。 */
                lcd_ui_cloud("注册过于频繁，稍后重试");
                ESP_LOGE(TAG, "注册频率超限，%d 秒后重试", PAIR_RETRY_S);
                vTaskDelay(pdMS_TO_TICKS(PAIR_RETRY_S * 1000));
                continue;
            }
            if (device_register()) {
                prov_rereg_note();   /* 只有成功才消耗配额 */
                break;
            }
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

    /* 自己管理重连：定时器建得出来就关 esp-mqtt 的自动重连。
     * 定时器建不出来时退回自动重连——那样没有指数退避，但至少能连上。 */
    bool manual_reconnect = false;
    if (!s_reconnect_timer) {
        const esp_timer_create_args_t rt = {
            .callback = reconnect_cb, .name = "mqtt_reconn",
        };
        manual_reconnect = esp_timer_create(&rt, &s_reconnect_timer) == ESP_OK;
    }

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
        .network.disable_auto_reconnect = manual_reconnect,
    };
    if (!manual_reconnect) {
        ESP_LOGW(TAG, "重连定时器创建失败，退回 esp-mqtt 自动重连");
    }
    s_mqtt = esp_mqtt_client_init(&cfg);
    esp_mqtt_client_register_event(s_mqtt, ESP_EVENT_ANY_ID, on_mqtt, NULL);
    esp_mqtt_client_start(s_mqtt);
}
