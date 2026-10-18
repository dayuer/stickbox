/*
 * 取帧、换帧、每分钟拼时钟带（T13b）。
 *
 * 三条硬规矩，都是这块板子上踩过的：
 *
 *   - **帧包不进内存。** 一个帧包约 218KB，设备稳态空闲堆才 141KB，历史最低
 *     80KB。所以 HTTP body 是边收边写 flash 的（1KB 读缓冲 + fp_write_chunk），
 *     从头到尾不存在「完整帧包在 RAM 里」这个时刻。
 *   - **开机先用旧帧。** flash 里有有效帧就立刻起播，再去拉新的。电视通着
 *     USB 供电，关机即断电，冷启动到出画面的时间是用户每天都要看一次的。
 *   - **esp_timer 回调里只能 xTaskCreate。** 所以这里一个常驻的小栈时钟任务
 *     （每分钟换一次带，纯 memcpy）+ 一个按需创建、跑完自删的取帧任务
 *     （要开 TLS，8KB 栈），而不是一个常驻的大栈任务。
 */
#include "framefetch.h"

#include <stdlib.h>
#include <string.h>
#include <strings.h>   /* strcasecmp：比对响应头名字 */
#include <time.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "esp_crt_bundle.h"
#include "esp_heap_caps.h"
#include "esp_http_client.h"
#include "esp_log.h"
#include "esp_system.h"
#include "esp_timer.h"

#include "cloud_client.h"
#include "cloud_creds.h"
#include "framepack.h"
#include "photo_io.h"
#include "provision.h"
#include "statusband.h"
#include "stream.h"

static const char *TAG = "framefetch";

/* HTTP 读缓冲。1KB：大缓冲买不到吞吐（瓶颈在 TLS 和 flash 写），
 * 却要和推流抢同一块堆。 */
#define CHUNK 1024

/* 开 TLS 大约要 40KB。门槛 90KB 是为了给「历史最低空闲堆 ≥40KB」留出
 * 硬余量：流在发帧时 WiFi 发送缓冲会周期性占用几十 KB，60KB 的旧门槛
 * 会让 TLS 握手把最低点打到几 KB（M2 实测 min=320）。堆低于门槛时
 * 先等最多 10 秒（发送缓冲释放），等不到再按退避重试。 */
#define MIN_FREE_HEAP 90000
#define NET_WAIT_MS   10000

#define RETRY_MIN_S  15
#define RETRY_MAX_S 300
/* 服务端说「还没配对」时的兜底轮询间隔。正常路径是配对成功时被叫一次，
 * 这条只防「配对通知丢了」。 */
#define UNPAIRED_RETRY_S 60

typedef enum {
    FETCH_OK,         /* 拿到新帧并已激活 */
    FETCH_SAME,       /* 304：服务端的帧和手里这个一样 */
    FETCH_UNPAIRED,   /* 403：还没配对，屏幕归配对码 */
    FETCH_FAIL,
} fetch_r;

/* 「正在取帧」的闸。三个地方会叫取帧（mqtt 任务、时钟任务、配对回调），
 * 所以用原子的 test-and-set 而不是 if(flag){flag=1}：两个任务同时挤进来会
 * 各自 fp_write_begin() 到同一个槽上，把帧包写成两份交错的字节。 */
static volatile bool s_fetching;
static volatile bool s_first_done;
static volatile bool s_again;      /* 取帧期间又收到通知 */
/* 取帧期间收到的通知版本。同一版本只算一次；收完再和已激活版本比对，
 * 相同就丢掉——避免开机时“取帧 + 两条 v9 通知”叠成三次 TLS。 */
static char    s_again_ver[32];
/* 当前这轮取帧的目标版本（通知触发时已知；开机/配对触发时为空）。 */
static char    s_target_ver[32];
static portMUX_TYPE s_ver_mux = portMUX_INITIALIZER_UNLOCKED;
static int     s_retry_s = RETRY_MIN_S;
static void    clear_target(void);
static int64_t s_next_try_us;      /* 0 = 不用重试 */
static int     s_last_min = -1;    /* 已发布时钟带对应的 HH*60+MM */
static bool    s_offline_shown;

static bool have_frame(void)
{
    return framepack_active(NULL, NULL, NULL, NULL, NULL);
}

/* 状态页和配对码抢的是同一段 MCU 行（stream_start() 只接受一组行范围）。
 * 配对码是用户正要照着输的东西，优先级更高——没配对时状态页一律不发。 */
static void show_status(status_id_t s)
{
    if (cloud_pair_pending()) return;
    statusband_show(s);
}

/* ------------------------------------------------------------ 时钟带 */

/* 按当前 HH:MM 拼时钟带并发布。返回是否真的发布了。 */
static bool publish_band(void)
{
    const fp_index_t *ix;
    const uint8_t *base, *atlas;
    size_t base_len, atlas_len;
    if (!framepack_active(&ix, &base, &base_len, &atlas, &atlas_len)) return false;

    time_t now = time(NULL);
    struct tm tm;
    localtime_r(&now, &tm);
    /* SNTP 还没对上时 time() 还在 1970（CST-8 下就是 08:00）。
     * 那就不发带：电视上是四张空白卡片，难看但诚实；显示一个错的时间
     * 比不显示糟得多。对上时间后这个函数自然就开始出数字。 */
    if (tm.tm_year + 1900 < 2024) return false;

    uint8_t *buf = stream_band_begin();
    size_t len = fp_build_band(ix, base, base_len, atlas, atlas_len,
                               tm.tm_hour, tm.tm_min, buf, stream_band_cap());
    if (!len) {
        ESP_LOGE(TAG, "拼 %02d:%02d 的时钟带失败（缓冲 %u B 不够或索引不合法）",
                 tm.tm_hour, tm.tm_min, (unsigned)stream_band_cap());
        return false;
    }
    uint32_t off[FP_BAND_ROWS], row_len[FP_BAND_ROWS];
    fp_band_layout(ix, off, row_len, tm.tm_hour, tm.tm_min);
    if (!stream_band_publish_rows(buf, len, off, row_len)) return false;

    s_last_min = tm.tm_hour * 60 + tm.tm_min;
    return true;
}

/* 把激活帧挂到推流上，并立刻出一版时钟带。 */
static void attach_active(void)
{
    const fp_index_t *ix;
    const uint8_t *base;
    size_t base_len;
    if (!framepack_active(&ix, &base, &base_len, NULL, NULL)) return;
    stream_use_framepack(ix, base, base_len);
    s_last_min = -1;        /* 强制重拼：旧的带是给上一张底帧拼的 */
    publish_band();
}

/* ------------------------------------------------------------ 取帧 */

/* 响应头里要的东西。只关心版本号——它要写进槽头部，下次开机和下次收通知
 * 都靠它比对。 */
typedef struct { char ver[32]; } hdrs_t;

static esp_err_t on_http_event(esp_http_client_event_t *e)
{
    if (e->event_id != HTTP_EVENT_ON_HEADER) return ESP_OK;
    hdrs_t *h = e->user_data;
    if (!h || !e->header_key || !e->header_value) return ESP_OK;

    if (strcasecmp(e->header_key, "X-Frame-Version") == 0) {
        strlcpy(h->ver, e->header_value, sizeof h->ver);
    } else if (strcasecmp(e->header_key, "ETag") == 0 && !h->ver[0]) {
        /* ETag 是带引号的版本号（形如 "7"）。扒掉引号，和 X-Frame-Version
         * 对齐——两边存的必须是同一个串，否则 If-None-Match 永远不命中。 */
        const char *v = e->header_value;
        if (*v == '"') v++;
        strlcpy(h->ver, v, sizeof h->ver);
        char *q = strchr(h->ver, '"');
        if (q) *q = 0;
    }
    return ESP_OK;
}

static fetch_r fetch_once(void)
{
    const char *key = cloud_devkey();
    if (!key[0]) {
        /* 还没注册完，没有 Bearer 可用。等下一轮。 */
        ESP_LOGI(TAG, "还没有设备密钥，本轮不取帧");
        return FETCH_FAIL;
    }

    unsigned heap = (unsigned)esp_get_free_heap_size();
    if (heap < MIN_FREE_HEAP) {
        ESP_LOGW(TAG, "空闲堆 %u < %d，本轮不取帧（推流优先）", heap, MIN_FREE_HEAP);
        return FETCH_FAIL;
    }

    char url[160];
    snprintf(url, sizeof url, "%s/device/frame", cloud_api_base());

    hdrs_t hd = { 0 };
    esp_http_client_config_t cfg = {
        .url = url,
        .method = HTTP_METHOD_GET,
        .timeout_ms = 20000,
        .buffer_size = CHUNK,
        .buffer_size_tx = 512,
        .event_handler = on_http_event,
        .user_data = &hd,
#ifndef STICKBOX_INSECURE_TLS
        .crt_bundle_attach = esp_crt_bundle_attach,
#endif
    };
    esp_http_client_handle_t h = esp_http_client_init(&cfg);
    if (!h) return FETCH_FAIL;

    fetch_r r = FETCH_FAIL;
    uint8_t *buf = NULL;
    fp_writer_t w = { 0 };
    bool writing = false;

    char auth[96];
    snprintf(auth, sizeof auth, "Bearer %s", key);
    esp_http_client_set_header(h, "Authorization", auth);

    /* 条件请求。MQTT 通知里的 ver 已经比对过一次，这是第二道：服务端没出
     * 新帧时直接 304，省掉 218KB 下载和一次 256KB 的槽擦除（flash 寿命）。 */
    const char *cur = framepack_version();
    char inm[40];
    if (cur[0]) {
        snprintf(inm, sizeof inm, "\"%s\"", cur);
        esp_http_client_set_header(h, "If-None-Match", inm);
    }

    if (esp_http_client_open(h, 0) != ESP_OK) {
        ESP_LOGW(TAG, "连不上 %s", url);
        goto done;
    }

    int64_t clen = esp_http_client_fetch_headers(h);
    int st = esp_http_client_get_status_code(h);

    if (st == 304) {
        ESP_LOGI(TAG, "服务端的帧还是版本 %s（304），不用重拉", cur);
        r = FETCH_SAME;
        goto done;
    }
    if (st == 403) {
        /* 待配对的密钥读不到帧。屏幕这时归配对码，别去覆盖它。 */
        ESP_LOGI(TAG, "服务端说这台还没配对（403），等配对完成");
        r = FETCH_UNPAIRED;
        goto done;
    }
    if (st != 200) {
        /* 404 = 服务端还没给这个用户出过帧，和网络错一样按退避重试。 */
        ESP_LOGW(TAG, "取帧失败 HTTP %d", st);
        goto done;
    }
    if (clen <= 0) {
        /* 没有 Content-Length（例如 chunked）就不知道帧包多长，而 fp_write_begin
         * 必须先知道长度才能定槽、算 CRC、拦住超长包。服务端是定长 body，
         * 走到这儿说明两边的约定变了——宁可整帧不要。 */
        ESP_LOGE(TAG, "响应没有 Content-Length（%lld），拒收", (long long)clen);
        goto done;
    }
    if (!hd.ver[0]) {
        ESP_LOGE(TAG, "响应没带版本号（X-Frame-Version / ETag），拒收");
        goto done;
    }
    if (strlen(hd.ver) > 12) {
        /* 槽头部只留了 12 字节放版本串（framepack.h 的布局）。更长的串写进去
         * 会被截断，于是重启后 If-None-Match 再也不可能命中——每次开机白下
         * 218KB，每条通知都白擦一个槽。帧本身照收（有画面比没画面重要），
         * 但这条必须吵，否则没人会发现。 */
        ESP_LOGE(TAG, "版本串 %s 超过槽头部的 12 字节，重启后条件请求会失效",
                 hd.ver);
    }

    buf = malloc(CHUNK);
    if (!buf) {
        ESP_LOGE(TAG, "连 %d 字节读缓冲都分不出来", CHUNK);
        goto done;
    }
    if (!fp_write_begin(&w, (uint32_t)clen, hd.ver)) goto done;
    writing = true;

    ESP_LOGI(TAG, "开始取帧：%lld 字节，版本 %s，槽 %d，堆 %u",
             (long long)clen, hd.ver, w.slot, heap);

    uint32_t got = 0;
    while (got < (uint32_t)clen) {
        int n = esp_http_client_read(h, (char *)buf, CHUNK);
        if (n <= 0) {
            ESP_LOGE(TAG, "下载中断，只收到 %u/%lld 字节", (unsigned)got, (long long)clen);
            goto done;
        }
        if (!fp_write_chunk(&w, buf, (size_t)n)) goto done;
        got += (uint32_t)n;
    }

    /* 旧底帧的 mmap 会在 fp_write_finish() 里被换掉，而推流任务可能正拿着
     * 那个地址在 send()。这里**不需要**先把推流切走：framepack.c 的
     * activate() 已经保证「先 stream_use_framepack()（内部等在途的帧发完）
     * 再 munmap」，是硬保证。
     *
     * 原先这里有个 drain_old_frame()：切到预置页 + 等帧计数前进两帧，最多
     * 4 秒。它作者自己标了「不是硬保证」——电视卡住不读时单次 send 能挂到
     * SO_SNDTIMEO 的 15 秒，而它只看得到总帧计数、分不清是哪个连接在前进。
     * 而且代价是每次换帧电视上有 0.5~1 秒纯色底。换成硬保证之后两个问题
     * 都没了：不切预置页，画面直接从旧看板跳到新看板。 */

    if (!fp_write_finish(&w)) goto done;
    writing = false;
    r = FETCH_OK;

done:
    if (writing) fp_write_abort(&w);
    free(buf);
    esp_http_client_cleanup(h);
    return r;
}

/* ------------------------------------------------------------ 任务 */

bool framefetch_busy(void)
{
    return __atomic_load_n(&s_fetching, __ATOMIC_ACQUIRE);
}

static void fetch_task(void *arg)
{
    (void)arg;
    /* MQTT 重连和取帧同时开 TLS 会把堆打到几 KB；MQTT 不在线时先不取。
     * 堆门槛放在「停流、排空发送缓冲」之后：板子发帧时稳态只有 65–80K，
     * 停流前判断只会一直失败。 */
    fetch_r r;
    if (!cloud_connected()) {
        ESP_LOGI(TAG, "MQTT 未连接，本轮不取帧");
        r = FETCH_FAIL;
    } else if (photo_io_net_lock(120000)) {
        stream_quiet(true);
        vTaskDelay(pdMS_TO_TICKS(1000));
        for (int i = 0; i < NET_WAIT_MS / 100 &&
                        esp_get_free_heap_size() < MIN_FREE_HEAP; i++)
            vTaskDelay(pdMS_TO_TICKS(100));
        if (esp_get_free_heap_size() < MIN_FREE_HEAP) {
            ESP_LOGW(TAG, "停流后堆仍只有 %u < %d，本轮不取帧",
                     (unsigned)esp_get_free_heap_size(), MIN_FREE_HEAP);
            r = FETCH_FAIL;
        } else {
            r = fetch_once();
        }
        stream_quiet(false);
        photo_io_net_unlock();
    } else {
        ESP_LOGW(TAG, "等照片任务放锁超时，本轮取帧跳过");
        r = FETCH_FAIL;
    }

    switch (r) {
    case FETCH_OK:
        attach_active();
        s_offline_shown = false;
        s_retry_s = RETRY_MIN_S;
        s_next_try_us = 0;
        ESP_LOGI(TAG, "新帧已上屏，版本 %s，堆 %u",
                 framepack_version(), (unsigned)esp_get_free_heap_size());
        break;

    case FETCH_SAME:
        s_retry_s = RETRY_MIN_S;
        s_next_try_us = 0;
        break;

    case FETCH_UNPAIRED:
        s_next_try_us = esp_timer_get_time() + (int64_t)UNPAIRED_RETRY_S * 1000000;
        break;

    case FETCH_FAIL:
    default:
        /* 手里有帧就继续用它：画面是看板、时钟还在走，比盖上一张「离线」
         * 有用得多。
         *
         * 取帧失败时推流其实**没有被动过**——删掉 drain_old_frame() 之后
         * 不再有「先切预置页」这一步，旧帧全程挂着。这里仍然叫一次
         * attach_active() 是为了幂等：它只是重新发布一版时钟带，代价很小，
         * 而万一哪条新路径真的把推流切走了，这一步能把画面救回来。 */
        if (have_frame()) {
            attach_active();
        } else if (!s_offline_shown) {
            show_status(STATUS_OFFLINE);
            s_offline_shown = true;
        }
        s_next_try_us = esp_timer_get_time() + (int64_t)s_retry_s * 1000000;
        ESP_LOGW(TAG, "%d 秒后重试取帧", s_retry_s);
        s_retry_s = s_retry_s * 2 > RETRY_MAX_S ? RETRY_MAX_S : s_retry_s * 2;
        break;
    }

    clear_target();
    /* 取帧期间收到过通知：只有通知的版本和刚激活的版本不同才补拉。
     * 开机时服务端常在同一次取帧里发两条同版本通知；不比版本就会白开
     * 第二次 TLS（历史最低堆 14KB 的主要来源之一）。 */
    if (__atomic_exchange_n(&s_again, false, __ATOMIC_ACQUIRE)) {
        if (strcmp(s_again_ver, framepack_version()) != 0) {
            s_next_try_us = esp_timer_get_time();
        } else {
            ESP_LOGI(TAG, "取帧期间的通知版本 %s 已激活，不再补拉", s_again_ver);
        }
    }
    bool first;
    taskENTER_CRITICAL(&s_ver_mux);
    first = !s_first_done;
    s_first_done = true;
    taskEXIT_CRITICAL(&s_ver_mux);
    if (first) {
        multi_heap_info_t in;
        heap_caps_get_info(&in, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
        ESP_LOGW(TAG, "首帧取帧结束：free=%u largest=%u min=%u",
                 (unsigned)in.total_free_bytes,
                 (unsigned)in.largest_free_block,
                 (unsigned)in.minimum_free_bytes);
    }
    __atomic_clear(&s_fetching, __ATOMIC_SEQ_CST);
    vTaskDelete(NULL);
}

bool framefetch_first_done(void)
{
    return __atomic_load_n(&s_first_done, __ATOMIC_ACQUIRE);
}

static void set_target(const char *ver)
{
    taskENTER_CRITICAL(&s_ver_mux);
    strlcpy(s_target_ver, ver ? ver : "", sizeof s_target_ver);
    taskEXIT_CRITICAL(&s_ver_mux);
}

static bool target_is(const char *ver)
{
    bool same;
    taskENTER_CRITICAL(&s_ver_mux);
    same = s_target_ver[0] && strcmp(s_target_ver, ver) == 0;
    taskEXIT_CRITICAL(&s_ver_mux);
    return same;
}

static void clear_target(void)
{
    taskENTER_CRITICAL(&s_ver_mux);
    s_target_ver[0] = 0;
    taskEXIT_CRITICAL(&s_ver_mux);
}

static void request_with_version(const char *why, const char *ver)
{
    if (__atomic_test_and_set(&s_fetching, __ATOMIC_SEQ_CST)) {
        /* 已经有任务在跑：只记下最新通知，收完再判断要不要补一次。 */
        if (ver && ver[0]) {
            strlcpy(s_again_ver, ver, sizeof s_again_ver);
            __atomic_store_n(&s_again, true, __ATOMIC_RELEASE);
            ESP_LOGI(TAG, "正在取帧，记下版本 %s 待比对", ver);
        }
        return;
    }
    set_target(ver);
    /* 8192：一次 TLS 握手，4096 太紧（ota.c 同款）。只在取帧期间占用。 */
    if (xTaskCreate(fetch_task, "framefetch", 8192, NULL, 3, NULL) != pdPASS) {
        clear_target();
        __atomic_clear(&s_fetching, __ATOMIC_SEQ_CST);
        /* 堆不够建任务。不打日志里的字符串拼接，留给下一轮退避重试。 */
        s_next_try_us = esp_timer_get_time() + (int64_t)RETRY_MIN_S * 1000000;
        return;
    }
    ESP_LOGI(TAG, "开始取帧：%s%s%s", why ? why : "",
             (ver && ver[0]) ? "，版本 " : "", (ver && ver[0]) ? ver : "");
}

void framefetch_request(const char *why)
{
    request_with_version(why, NULL);
}

void framefetch_notify(const char *ver)
{
    if (!ver || !ver[0]) return;
    if (strcmp(ver, framepack_version()) == 0) {
        ESP_LOGI(TAG, "通知的版本 %s 就是手里这张，不重拉", ver);
        return;
    }
    if (target_is(ver)) {
        ESP_LOGI(TAG, "正在取的就是版本 %s，忽略重复通知", ver);
        return;
    }
    if (s_fetching) {
        strlcpy(s_again_ver, ver, sizeof s_again_ver);
        __atomic_store_n(&s_again, true, __ATOMIC_RELEASE);
        ESP_LOGI(TAG, "正在取上一版，收完再比对（版本 %s）", ver);
        return;
    }
    request_with_version("收到帧版本通知", ver);
}

/* 时钟任务：每分钟换一次时钟带，顺便看着取帧的退避。
 *
 * 栈 4096：拼带本身只是 memcpy，但 statusband 那条路会走进 jb 的编码器
 * （enc_block 里有两块 256 字节的局部数组），再加上日志格式化。 */
static void tick_task(void *arg)
{
    (void)arg;
    bool first = true;

    while (1) {
        if (first) {
            first = false;
            /* flash 里一张帧都没有：电视上先出「连接中…」，别让用户对着
             * 一张纯色图猜。注意这步必须在这个任务里做而不是在 app_main
             * 里——主任务栈只有 3.5KB，jb 的编码器进去很勉强。 */
            if (!have_frame()) show_status(STATUS_CONNECTING);
        }

        time_t now = time(NULL);
        struct tm tm;
        localtime_r(&now, &tm);
        if (tm.tm_hour * 60 + tm.tm_min != s_last_min) publish_band();

        /* 只在排了重试的时候才拉。顺利的时候 s_next_try_us 是 0，这里一次
         * TLS 都不开——内容每分钟最多变一次，轮询买不到任何新鲜度。
         * 排重试的只有三种情况：取帧失败、还没配对、取帧期间又来了通知。 */
        if (s_next_try_us && esp_timer_get_time() >= s_next_try_us) {
            s_next_try_us = 0;
            framefetch_request("还没有可用的帧包");
        }

        vTaskDelay(pdMS_TO_TICKS(1000));
    }
}

void framefetch_boot(void)
{
    /* 没有设备密钥（首次开机、解绑后、复位前没来得及擦槽）：flash 里的帧
     * 属于上一个主人，绝不能起播。先擦掉再走配对页。 */
    char key[80];
    if (!prov_load_devkey(key, sizeof key)) {
        ESP_LOGW(TAG, "无设备密钥：作废 flash 帧，不从旧帧起播");
        framepack_invalidate();
    } else if (framepack_init()) {
        /* 扫 frames 分区，挑 seq 最大的有效槽。没有就返回 false——
         * 那说明这是第一次开机，或者上次的帧写坏了。 */
        attach_active();
        ESP_LOGI(TAG, "用 flash 里的帧起播，版本 %s", framepack_version());
    } else {
        ESP_LOGI(TAG, "flash 里没有可用帧，等服务端下发");
    }
    xTaskCreate(tick_task, "frametick", 4096, NULL, 3, NULL);
}

void framefetch_start(void)
{
    framefetch_request("开机");
}
