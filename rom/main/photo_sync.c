/*
 * 照片同步（M2）。设计约束（docs/PHOTO.md、USB-TV 的验收口径）：
 *
 *   - 清单只在 plv 变化 / 首次连接 / 卡从不可用变可用时拉；
 *     ETag: "<plv>"，304 不重下；整份 ≤64KB，边读边解，不进大堆。
 *   - 只认 init_token + init_expires_at + init_force；SNTP 未同步、
 *     缺到期时间、token 用过，一律不格式化。
 *   - 下载按清单顺序补缺；Range 只用于本次开机的续传（偏移在 RAM）。
 *     写完 sha256 读回校验由 photostore_put_finish 完成，索引项是提交点。
 *   - 取帧优先：每读 4KB 检查 framefetch_busy()，有取帧就断流、放锁、
 *     保留偏移，下次 Range 续。
 *   - status.photo 每次心跳上报；这里只维护快照，格式化由心跳任务读。
 */
#include "photo_sync.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"

#include "esp_crt_bundle.h"
#include "esp_heap_caps.h"
#include "esp_http_client.h"
#include "esp_log.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "nvs.h"

#include "cloud_client.h"
#include "framefetch.h"
#include "photo_io.h"
#include "photo_json.h"
#include "photo_play.h"
#include "photo_policy.h"
#include "photo_sd.h"
#include "photostore.h"
#include "stream.h"

static const char *TAG = "photo";

#define TASK_STACK       8192
#define TASK_PRIO        2
#define READ_CHUNK       2048   /* HTTP 读缓冲；解析器/写卡都按任意分片处理 */
#define PL_MAX_BYTES     65536
/* 网络操作的堆门槛：下载/清单会再开一次 TLS，必须给 40KB 硬底线留余量。
 * 低于门槛时任务先暂停照片播放、等堆回到门槛以上再开连接。 */
#define MIN_FREE_HEAP    90000
#define NET_WAIT_MS      10000
#define FRAME_WAIT_MS    5000
/* 清单拉取持 net 时，如果取帧在等：最多再占 500ms 就断流让路。
 * 加一次 HTTPS 连接阶段（正常 <1s），看板帧被挡的上界约 1.5s。
 * 见 docs/PHOTO-M3-P0.md 第 3 节。 */
#define FRAME_YIELD_MS   500
#define FETCH_FAIL_US    (60LL * 1000 * 1000)
#define RETRY_SOON_US    (1000LL * 1000)
#define RETRY_MAX_US     (1800LL * 1000 * 1000)

static photo_pl_t   s_pl;
static photo_json_t s_pj;
static bool         s_have_pl;
/* 保护 s_pl 的写（拉取）与播放侧的逐项访问。只在清单拉取期间持锁，
 * 播放调度最多等一次 HTTPS GET。 */
static SemaphoreHandle_t s_pl_mux;
static volatile uint32_t s_play_gen;
static int          s_last_plv = -1;
static volatile int s_want_plv = -1;
static volatile bool s_force_fetch;
static volatile bool s_boot_mount;
static volatile bool s_formatting;
static volatile bool s_running;
static volatile bool s_kick;
static int          s_fails[PHOTO_PL_MAX_PHOTOS];
static int64_t      s_next_try_us[PHOTO_PL_MAX_PHOTOS];
static bool         s_ready_prev;
static char         s_used_token[65];
static uint8_t      s_buf[READ_CHUNK];

/* 本次开机内未提交的半成品。掉电后从头下——不信任半写数据。 */
static ps_put_t    *s_put;
static uint8_t      s_put_sha[32];
static uint32_t     s_put_off, s_put_size;
static bool         s_put_live;

static esp_timer_handle_t s_retry_timer;

typedef struct {
    bool        ready;
    int         plv;
    int         have;
    uint64_t    free_kb;
    const char *state;
    char        init_done[9];
    photo_bad_t bad;
} snap_t;

static snap_t       s_snap;
static portMUX_TYPE s_mux = portMUX_INITIALIZER_UNLOCKED;

static void kick(void);
static void photo_task(void *arg);

/* ------------------------------------------------------------ 小工具 */

static void sha_to_hex(const uint8_t sha[32], char out[65])
{
    static const char hx[] = "0123456789abcdef";
    for (int i = 0; i < 32; i++) {
        out[i * 2] = hx[sha[i] >> 4];
        out[i * 2 + 1] = hx[sha[i] & 0xF];
    }
    out[64] = 0;
}

static const photo_pl_entry_t *pl_find(const photo_pl_t *pl,
                                       const uint8_t sha[32])
{
    for (int i = 0; i < pl->photo_count; i++)
        if (memcmp(pl->photos[i].sha256, sha, 32) == 0) return &pl->photos[i];
    return NULL;
}

static bool wall_time_valid(time_t *out)
{
    time_t now = time(NULL);
    struct tm tm;
    localtime_r(&now, &tm);
    if (out) *out = now;
    return tm.tm_year + 1900 >= 2024;
}

/* ------------------------------------------------------------ NVS */

static bool nvs_load_used(char *out, size_t cap)
{
    nvs_handle_t h;
    if (nvs_open("photo", NVS_READONLY, &h) != ESP_OK) return false;
    size_t n = cap;
    bool ok = nvs_get_str(h, "used_token", out, &n) == ESP_OK && out[0];
    nvs_close(h);
    return ok;
}

static bool nvs_save_used(const char *tok)
{
    nvs_handle_t h;
    if (nvs_open("photo", NVS_READWRITE, &h) != ESP_OK) return false;
    esp_err_t e = nvs_set_str(h, "used_token", tok);
    if (e == ESP_OK) e = nvs_commit(h);
    nvs_close(h);
    return e == ESP_OK;
}

static bool token_used(const char *tok)
{
    /* 服务端每次 photo init 都会换新 token；用前 8 位比对足够，
     * 而且和上报的 init_done 是同一个口径。 */
    return s_used_token[0] && strncmp(s_used_token, tok, 8) == 0;
}

/* ------------------------------------------------------------ 快照 */

static void set_card(photo_state_t st)
{
    taskENTER_CRITICAL(&s_mux);
    s_snap.ready = st == PHOTO_READY;
    s_snap.state = photo_state_name(st);
    if (st != PHOTO_READY) {
        s_snap.have = 0;
        s_snap.free_kb = 0;
    }
    taskEXIT_CRITICAL(&s_mux);
}

static void set_plv(int plv)
{
    taskENTER_CRITICAL(&s_mux);
    s_snap.plv = plv;
    taskEXIT_CRITICAL(&s_mux);
}

static void update_stats(photostore_t *store)
{
    if (!store) return;
    uint32_t bad = 0;
    uint64_t free_b = 0, total = 0;
    uint16_t used = 0;
    if (!photo_io_store_lock(30000)) return;
    int valid = photostore_scan_index(store, &bad);
    photostore_space(store, &free_b, &total, &used);
    photo_io_store_unlock();
    taskENTER_CRITICAL(&s_mux);
    s_snap.have = valid;
    s_snap.free_kb = free_b / 1024;
    taskEXIT_CRITICAL(&s_mux);
}

static void bad_add(const char hex[65])
{
    taskENTER_CRITICAL(&s_mux);
    photo_bad_add(&s_snap.bad, hex);
    taskEXIT_CRITICAL(&s_mux);
}

static void set_init_done(const char *tok)
{
    taskENTER_CRITICAL(&s_mux);
    memcpy(s_snap.init_done, tok, 8);
    s_snap.init_done[8] = 0;
    taskEXIT_CRITICAL(&s_mux);
}

static void prune_bad(void)
{
    photo_bad_t nb = { 0 };
    for (int i = 0; i < s_snap.bad.count; i++) {
        bool keep = false;
        for (int k = 0; k < s_pl.photo_count && !keep; k++) {
            char hex[65];
            sha_to_hex(s_pl.photos[k].sha256, hex);
            keep = memcmp(hex, s_snap.bad.prefix[i], 8) == 0;
        }
        if (keep && nb.count < PHOTO_BAD_MAX) {
            memcpy(nb.prefix[nb.count], s_snap.bad.prefix[i], 9);
            nb.count++;
        }
    }
    taskENTER_CRITICAL(&s_mux);
    s_snap.bad = nb;
    taskEXIT_CRITICAL(&s_mux);
}

/* ------------------------------------------------------------ 退避 */

static void on_retry_timer(void *arg)
{
    (void)arg;
    kick();
}

static void schedule_retry(int64_t at_us)
{
    if (!s_retry_timer) return;
    esp_timer_stop(s_retry_timer);   /* 没在跑时返回错误，忽略 */
    int64_t delay = at_us - esp_timer_get_time();
    if (delay < 100000) delay = 100000;
    if (delay > RETRY_MAX_US) delay = RETRY_MAX_US;
    esp_timer_start_once(s_retry_timer, (uint64_t)delay);
}

static void cancel_retry(void)
{
    if (s_retry_timer) esp_timer_stop(s_retry_timer);
}

/* ------------------------------------------------------------ 清单 */

/* 返回 0 成功（*notmod 表示 304）、-1 失败、-2 未配对。 */
static int fetch_playlist(bool *notmod)
{
    *notmod = false;
    const char *key = cloud_devkey();
    if (!key[0]) {
        ESP_LOGI(TAG, "还没有设备密钥，不拉清单");
        return -1;
    }
    const char *base = cloud_api_base();
    if (!base[0]) {
        /* cloud_client_start() 之前（开机挂卡那一轮）还没有 API 地址。 */
        ESP_LOGI(TAG, "API 地址还没就绪，稍后再拉清单");
        return -1;
    }
    /* 堆门槛放到 quiet 排空之后（见下），这里只看取帧。 */
    if (framefetch_busy()) return -3;

    char url[160];
    snprintf(url, sizeof url, "%s/device/playlist", base);

    esp_http_client_config_t cfg = {
        .url = url,
        .method = HTTP_METHOD_GET,
        .timeout_ms = 20000,
        .buffer_size = READ_CHUNK,
        .buffer_size_tx = 512,
#ifndef STICKBOX_INSECURE_TLS
        .crt_bundle_attach = esp_crt_bundle_attach,
#endif
    };
    esp_http_client_handle_t h = esp_http_client_init(&cfg);
    if (!h) return -1;

    char auth[96];
    snprintf(auth, sizeof auth, "Bearer %s", key);
    esp_http_client_set_header(h, "Authorization", auth);
    if (s_last_plv >= 0) {
        char inm[24];
        snprintf(inm, sizeof inm, "\"%d\"", s_last_plv);
        esp_http_client_set_header(h, "If-None-Match", inm);
    }

    if (!photo_io_net_lock(30000)) {
        esp_http_client_cleanup(h);
        return -1;
    }
    /* 停发几帧，排空 WiFi 发送缓冲再握手；期间照片播放已暂停。 */
    stream_quiet(true);
    vTaskDelay(pdMS_TO_TICKS(1000));
    for (int i = 0; i < NET_WAIT_MS / 100 &&
                    esp_get_free_heap_size() < MIN_FREE_HEAP; i++)
        vTaskDelay(pdMS_TO_TICKS(100));
    if (esp_get_free_heap_size() < MIN_FREE_HEAP) {
        ESP_LOGW(TAG, "停流后堆仍只有 %u < %d，清单延后",
                 (unsigned)esp_get_free_heap_size(), MIN_FREE_HEAP);
        stream_quiet(false);
        photo_io_net_unlock();
        esp_http_client_cleanup(h);
        return -3;
    }
    ESP_LOGI(TAG, "清单 TLS 开始，堆 %u 最大块 %u",
             (unsigned)esp_get_free_heap_size(),
             (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL |
                                                        MALLOC_CAP_8BIT));

    int64_t lock_t0 = esp_timer_get_time();
    bool pl_locked = false;
    int ret = -1;
    if (esp_http_client_open(h, 0) != ESP_OK) {
        ESP_LOGW(TAG, "连不上 %s", url);
        goto done;
    }

    int64_t clen = esp_http_client_fetch_headers(h);
    int st = esp_http_client_get_status_code(h);
    if (st == 304) {
        *notmod = true;
        ret = 0;
        goto done;
    }
    if (st == 403) {
        ret = -2;
        goto done;
    }
    if (st != 200) {
        ESP_LOGW(TAG, "拉清单失败 HTTP %d", st);
        goto done;
    }
    if (framefetch_busy() &&
        esp_timer_get_time() - lock_t0 > (int64_t)FRAME_YIELD_MS * 1000) {
        ESP_LOGW(TAG, "取帧在等，清单让路（连接阶段已超 %d ms）", FRAME_YIELD_MS);
        ret = -3;
        goto done;
    }
    if (clen > PL_MAX_BYTES) {
        ESP_LOGE(TAG, "清单 %lld 字节超过 64KB 上限，拒收", (long long)clen);
        goto done;
    }

    if (s_pl_mux) xSemaphoreTake(s_pl_mux, portMAX_DELAY);
    pl_locked = true;
    s_have_pl = false;
    photo_json_init(&s_pj, &s_pl);
    uint32_t total = 0;
    for (;;) {
        if (framefetch_busy() &&
            esp_timer_get_time() - lock_t0 > (int64_t)FRAME_YIELD_MS * 1000) {
            ESP_LOGW(TAG, "取帧在等，清单拉取中断让路（已收 %u 字节）",
                     (unsigned)total);
            ret = -3;
            goto done;
        }
        int n = esp_http_client_read(h, (char *)s_buf, sizeof s_buf);
        if (n < 0) {
            ESP_LOGW(TAG, "清单读取中断（已收 %u 字节）", (unsigned)total);
            goto done;
        }
        if (n == 0) break;
        total += (uint32_t)n;
        if (total > PL_MAX_BYTES) {
            ESP_LOGE(TAG, "清单超过 64KB，中止");
            goto done;
        }
        if (!photo_json_feed(&s_pj, s_buf, (size_t)n)) {
            ESP_LOGE(TAG, "清单 JSON 非法（偏移 %u）", (unsigned)total);
            goto done;
        }
    }
    if (clen >= 0 && total != (uint32_t)clen) {
        ESP_LOGW(TAG, "清单长度不符：收到 %u / 声明 %lld",
                 (unsigned)total, (long long)clen);
        goto done;
    }
    if (!photo_json_finish(&s_pj) || !s_pl.has_plv || !s_pl.entries_seen) {
        ESP_LOGE(TAG, "清单不完整或缺少 plv/entries");
        goto done;
    }
    s_have_pl = true;
    ret = 0;

done:
    if (pl_locked && s_pl_mux) xSemaphoreGive(s_pl_mux);
    esp_http_client_close(h);
    stream_quiet(false);
    photo_io_net_unlock();
    esp_http_client_cleanup(h);
    ESP_LOGI(TAG, "清单 TLS 结束，堆 %u 最大块 %u",
             (unsigned)esp_get_free_heap_size(),
             (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL |
                                                        MALLOC_CAP_8BIT));
    return ret;
}

/* ------------------------------------------------------------ 初始化授权 */

static bool try_format(void)
{
    if (!s_pl.has_token || !photo_token_valid(s_pl.init_token)) return false;

    photo_format_mode_t mode = photo_sd_format_mode();
    time_t now = 0;
    bool tv = wall_time_valid(&now);
    photo_fmt_action_t act = photo_format_action(
        mode, true, token_used(s_pl.init_token), tv, (int64_t)now,
        s_pl.has_expires, s_pl.expires_at, s_pl.init_force);

    if (act == PHOTO_FMT_NO) {
        ESP_LOGW(TAG, "不格式化：mode=%d force=%d 时间有效=%d 有到期=%d 已用过=%d",
                 (int)mode, s_pl.init_force ? 1 : 0, tv ? 1 : 0,
                 s_pl.has_expires ? 1 : 0, token_used(s_pl.init_token) ? 1 : 0);
        return false;
    }

    bool force = act == PHOTO_FMT_GO_FORCE;
    ESP_LOGW(TAG, "凭 init_token 格式化（mode=%d force=%d）", (int)mode,
             force ? 1 : 0);

    /* 通知播放器立刻切回 board，并等正在发的那一帧结束再动卡。 */
    s_formatting = true;
    for (int i = 0; i < 200 && photo_play_holds() > 0; i++)
        vTaskDelay(pdMS_TO_TICKS(100));

    if (!photo_io_store_lock(30000)) {
        s_formatting = false;
        return false;
    }
    bool ok = photo_sd_format(true, force);
    photo_io_store_unlock();
    s_formatting = false;
    if (!ok) {
        ESP_LOGE(TAG, "格式化失败");
        return false;
    }

    strlcpy(s_used_token, s_pl.init_token, sizeof s_used_token);
    if (!nvs_save_used(s_used_token))
        ESP_LOGW(TAG, "记下已用 token 失败（重启后可能重复使用）");
    set_init_done(s_used_token);
    __atomic_add_fetch(&s_play_gen, 1, __ATOMIC_RELEASE);
    ESP_LOGW(TAG, "格式化完成，init_done=%.8s", s_used_token);
    return true;
}

/* ------------------------------------------------------------ GC / 元数据 */

static const photo_pl_t *s_gc_pl;
typedef struct { photostore_t *store; bool changed; } gc_ctx_t;

static bool gc_cb(const photo_entry_t *e, void *arg)
{
    gc_ctx_t *c = arg;
    const photo_pl_entry_t *p = pl_find(s_gc_pl, e->sha256);
    if (!p) {
        char hex[65];
        sha_to_hex(e->sha256, hex);
        if (photo_play_is_held(e->sha256)) {
            /* 有连接正在发这张：留着，下一次 GC 再收。 */
            ESP_LOGI(TAG, "照片 %.8s 正在播放，暂不回收", hex);
            return true;
        }
        ESP_LOGI(TAG, "回收不在清单里的照片 %.8s", hex);
        photostore_remove(c->store, e->sha256);
        c->changed = true;
        return true;
    }
    if (p->duration_s != e->duration_s || p->order != e->order) {
        photostore_update_meta(c->store, e->sha256, p->duration_s, p->order);
        c->changed = true;
    }
    return true;
}

static void gc_store(photostore_t *store)
{
    if (!photo_io_store_lock(30000)) return;
    s_gc_pl = &s_pl;
    uint32_t bad = 0;
    gc_ctx_t ctx = { .store = store, .changed = false };
    photostore_walk(store, gc_cb, &ctx, &bad);
    s_gc_pl = NULL;
    photo_io_store_unlock();
    /* 只有真的删/改了索引才让播放器重建，避免堆不足时每几秒重建一次。 */
    if (ctx.changed) __atomic_add_fetch(&s_play_gen, 1, __ATOMIC_RELEASE);
}

/* ------------------------------------------------------------ 下载 */

static void abort_put(void)
{
    if (s_put_live && s_put) photostore_put_abort(s_put);
    s_put = NULL;
    s_put_live = false;
    s_put_off = 0;
    s_put_size = 0;
}

enum { DL_OK, DL_RETRY, DL_PREEMPT, DL_RESTART, DL_NOSPACE };

static int download_one(const photo_pl_entry_t *e, photostore_t *store)
{
    int restarted = 0;
    char hex[65];
    sha_to_hex(e->sha256, hex);

begin:
    if (s_put_live && memcmp(s_put_sha, e->sha256, 32) != 0) abort_put();
    if (!s_put_live) {
        bool exists = false;
        /* 索引读写共用 photostore 的 4KB 缓冲，必须在互斥闸里。 */
        if (!photo_io_store_lock(30000)) return DL_PREEMPT;
        bool ok = photostore_put_begin(store, e->sha256, e->size, e->duration_s,
                                       e->order, &s_put, &exists);
        photo_io_store_unlock();
        if (!ok) {
            ESP_LOGW(TAG, "照片 %.8s 无法分配（卡满/索引满），跳过", hex);
            return DL_NOSPACE;
        }
        if (exists) return DL_OK;
        memcpy(s_put_sha, e->sha256, 32);
        s_put_off = 0;
        s_put_size = e->size;
        s_put_live = true;
    } else if (s_put_size != e->size) {
        abort_put();
        return DL_RETRY;
    }

    uint32_t off = s_put_off;
    if (off >= e->size) goto finish;   /* 上次已经收满，只差提交 */

    /* 堆门槛在 quiet 排空之后（见下）：板子发帧时稳态只有 65–80K，
     * 在停流之前判断永远不够。 */

    /* 取帧优先：等它做完再开自己的 TLS。 */
    for (int i = 0; i < FRAME_WAIT_MS / 100 && framefetch_busy(); i++)
        vTaskDelay(pdMS_TO_TICKS(100));
    if (framefetch_busy()) return DL_PREEMPT;

    char url[256], auth[96];
    snprintf(url, sizeof url, "%s/device/photo/%s", cloud_api_base(), hex);
    snprintf(auth, sizeof auth, "Bearer %s", cloud_devkey());

    esp_http_client_config_t cfg = {
        .url = url,
        .method = HTTP_METHOD_GET,
        .timeout_ms = 20000,
        .buffer_size = READ_CHUNK,
        .buffer_size_tx = 512,
#ifndef STICKBOX_INSECURE_TLS
        .crt_bundle_attach = esp_crt_bundle_attach,
#endif
    };
    esp_http_client_handle_t h = esp_http_client_init(&cfg);
    if (!h) return DL_RETRY;
    esp_http_client_set_header(h, "Authorization", auth);
    if (off > 0) {
        char rng[32];
        snprintf(rng, sizeof rng, "bytes=%u-", (unsigned)off);
        esp_http_client_set_header(h, "Range", rng);
        ESP_LOGI(TAG, "续传 %.8s：从 %u 字节开始", hex, (unsigned)off);
    }

    if (!photo_io_net_lock(30000)) {
        esp_http_client_cleanup(h);
        return DL_PREEMPT;
    }
    stream_quiet(true);
    vTaskDelay(pdMS_TO_TICKS(1000));
    for (int i = 0; i < NET_WAIT_MS / 100 &&
                    esp_get_free_heap_size() < MIN_FREE_HEAP; i++)
        vTaskDelay(pdMS_TO_TICKS(100));
    if (esp_get_free_heap_size() < MIN_FREE_HEAP) {
        ESP_LOGW(TAG, "停流后堆仍只有 %u < %d，下载延后",
                 (unsigned)esp_get_free_heap_size(), MIN_FREE_HEAP);
        stream_quiet(false);
        photo_io_net_unlock();
        esp_http_client_cleanup(h);
        return DL_PREEMPT;
    }
    ESP_LOGI(TAG, "下载 TLS 开始 %.8s，堆 %u 最大块 %u", hex,
             (unsigned)esp_get_free_heap_size(),
             (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL |
                                                        MALLOC_CAP_8BIT));
    if (esp_http_client_open(h, 0) != ESP_OK) {
        stream_quiet(false);
        photo_io_net_unlock();
        esp_http_client_cleanup(h);
        ESP_LOGW(TAG, "下载 %.8s 连不上", hex);
        return DL_RETRY;
    }

    int64_t clen = esp_http_client_fetch_headers(h);
    int st = esp_http_client_get_status_code(h);

    if (off > 0 && (st == 200 || st == 416)) {
        /* 服务端忽略了 Range（或偏移越界）：这次开机内从头再来一次。 */
        esp_http_client_close(h);
        stream_quiet(false);
        ESP_LOGI(TAG, "下载 TLS 结束，堆 %u 最大块 %u",
             (unsigned)esp_get_free_heap_size(),
             (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL |
                                                        MALLOC_CAP_8BIT));
        photo_io_net_unlock();
        esp_http_client_cleanup(h);
        abort_put();
        if (!restarted) {
            restarted = 1;
            goto begin;
        }
        return DL_RETRY;
    }
    if (!((off == 0 && st == 200) || (off > 0 && st == 206))) {
        esp_http_client_close(h);
        stream_quiet(false);
        ESP_LOGI(TAG, "下载 TLS 结束，堆 %u 最大块 %u",
             (unsigned)esp_get_free_heap_size(),
             (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL |
                                                        MALLOC_CAP_8BIT));
        photo_io_net_unlock();
        esp_http_client_cleanup(h);
        ESP_LOGW(TAG, "下载 %.8s HTTP %d", hex, st);
        return DL_RETRY;
    }
    if (clen <= 0 || (int64_t)off + clen != (int64_t)e->size) {
        esp_http_client_close(h);
        stream_quiet(false);
        ESP_LOGI(TAG, "下载 TLS 结束，堆 %u 最大块 %u",
             (unsigned)esp_get_free_heap_size(),
             (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL |
                                                        MALLOC_CAP_8BIT));
        photo_io_net_unlock();
        esp_http_client_cleanup(h);
        ESP_LOGW(TAG, "下载 %.8s 长度不符：offset=%u clen=%lld size=%u",
                 hex, (unsigned)off, (long long)clen, (unsigned)e->size);
        return DL_RETRY;
    }

    bool fatal = false, interrupted = false, preempted = false;
    while (off < e->size) {
        int n = esp_http_client_read(h, (char *)s_buf, sizeof s_buf);
        if (n <= 0) {
            interrupted = true;
            break;
        }
        if ((uint32_t)n > e->size - off) {
            fatal = true;
            break;
        }
        /* 持 net 时按锁序再拿 store 写一个 4KB 块；播放读卡也拿 store，
         * 所以两边最多互相等一个块。 */
        if (!photo_io_store_lock(30000)) {
            interrupted = true;
            break;
        }
        bool wok = photostore_put_write(s_put, s_buf, (uint32_t)n);
        photo_io_store_unlock();
        if (!wok) {
            fatal = true;
            break;
        }
        off += (uint32_t)n;
        s_put_off = off;
        /* 取帧请求来了：断流、保留偏移，让出锁。下次 Range 接着下。 */
        if (off < e->size && framefetch_busy()) {
            preempted = true;
            break;
        }
    }

    esp_http_client_close(h);
    stream_quiet(false);
    ESP_LOGI(TAG, "下载 TLS 结束，堆 %u 最大块 %u",
             (unsigned)esp_get_free_heap_size(),
             (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL |
                                                        MALLOC_CAP_8BIT));
    photo_io_net_unlock();
    esp_http_client_cleanup(h);

    if (fatal) {
        abort_put();
        return DL_RETRY;
    }
    if (preempted) {
        ESP_LOGI(TAG, "取帧优先，暂停 %.8s（已下 %u/%u）", hex,
                 (unsigned)off, (unsigned)e->size);
        return DL_PREEMPT;
    }
    if (interrupted) {
        ESP_LOGW(TAG, "下载 %.8s 中断（%u/%u），稍后续传", hex,
                 (unsigned)off, (unsigned)e->size);
        return DL_RETRY;
    }
    if (off != e->size) return DL_RETRY;

finish:
    if (!photo_io_store_lock(30000)) return DL_RETRY;
    bool ok = photostore_put_finish(s_put);
    photo_io_store_unlock();
    s_put = NULL;
    s_put_live = false;
    s_put_off = 0;
    s_put_size = 0;
    if (!ok) {
        ESP_LOGE(TAG, "照片 %.8s 写入校验失败（sha 不符/写卡错误）", hex);
        return DL_RETRY;
    }
    ESP_LOGI(TAG, "照片下载完成 %.8s（%u 字节）", hex, (unsigned)e->size);
    return DL_OK;
}

static void download_missing(photostore_t *store)
{
    int64_t next = 0;
    for (int i = 0; i < s_pl.photo_count; i++) {
        const photo_pl_entry_t *e = &s_pl.photos[i];
        char hex[65];
        sha_to_hex(e->sha256, hex);

        if (photo_bad_has(&s_snap.bad, hex)) continue;

        photo_file_t f;
        if (!photo_io_store_lock(30000)) return;
        bool have = photostore_get(store, e->sha256, &f);
        photo_io_store_unlock();
        if (have) continue;

        /* 新 plv 到了：立刻收工，别拿旧清单继续下。 */
        if (__atomic_load_n(&s_want_plv, __ATOMIC_ACQUIRE) != s_last_plv) return;

        int64_t now = esp_timer_get_time();
        if (s_next_try_us[i] > now) {
            if (!next || s_next_try_us[i] < next) next = s_next_try_us[i];
            continue;
        }

        int r = download_one(e, store);
        if (r == DL_OK) {
            s_fails[i] = 0;
            s_next_try_us[i] = 0;
            /* 新照片已提交：让播放器马上能把它排进序列。 */
            __atomic_add_fetch(&s_play_gen, 1, __ATOMIC_RELEASE);
            /* 一轮只下一张：维护窗口（停流握手）一次只有几秒，下完
             * 立刻恢复播放，1 秒后再为下一张开一个窗口。 */
            int64_t t = esp_timer_get_time() + RETRY_SOON_US;
            if (!next || t < next) next = t;
            break;
        }
        if (r == DL_NOSPACE) {
            /* 卡满不会自己变好；按清单顺序跳过并报到 bad。 */
            bad_add(hex);
            s_next_try_us[i] = 0;
            continue;
        }
        if (r == DL_PREEMPT) {
            int64_t t = esp_timer_get_time() + RETRY_SOON_US;
            s_next_try_us[i] = t;
            if (!next || t < next) next = t;
            break;
        }
        if (r == DL_RESTART) {
            int64_t t = esp_timer_get_time() + 100000;
            s_next_try_us[i] = 0;
            if (!next || t < next) next = t;
            break;
        }

        s_fails[i]++;
        int bs = photo_backoff_s(s_fails[i]);
        if (bs < 0) {
            ESP_LOGW(TAG, "照片 %.8s 连续三次失败，标记跳过", hex);
            bad_add(hex);
            s_next_try_us[i] = 0;
            abort_put();
        } else {
            int64_t t = esp_timer_get_time() + (int64_t)bs * 1000000LL;
            s_next_try_us[i] = t;
            if (!next || t < next) next = t;
        }
    }

    if (next) schedule_retry(next);
    else cancel_retry();
}

/* ------------------------------------------------------------ 应用清单 */

/* 清单里有未过期的 init_token。即使一张照片都没有，也要挂卡：
 * 用户标准流程是「先 photo init 再 photo add」，不能等到有照片才格式化。 */
static bool token_pending(void)
{
    if (!s_pl.has_token || !photo_token_valid(s_pl.init_token)) return false;
    time_t now;
    if (!wall_time_valid(&now)) return false;
    if (!s_pl.has_expires) return false;
    return (int64_t)now < s_pl.expires_at;
}

/* 挂载，必要时凭 token 格式化。true = READY（并已更新 status 快照）。 */
static bool mount_card_ready(void)
{
    if (!photo_io_store_lock(30000)) {
        schedule_retry(esp_timer_get_time() + RETRY_SOON_US);
        return false;
    }
    photo_state_t st = photo_sd_mount();
    photo_io_store_unlock();

    if (st != PHOTO_READY) {
        /* 卡要重新初始化/格式化：半成品数据区马上就不属于我们了。 */
        abort_put();
        if (try_format()) st = photo_sd_state();
    }
    if (st != PHOTO_READY) {
        ESP_LOGW(TAG, "SD 不可用（state=%d），本轮只显示看板", (int)st);
        set_card(st);
        s_ready_prev = false;
        return false;
    }
    set_card(PHOTO_READY);
    return true;
}

static void note_ready_transition(void)
{
    if (!s_ready_prev) {
        s_ready_prev = true;
        /* 卡从不可用变可用：按约定主动重拉一次清单，不依赖 plv 变化。 */
        __atomic_store_n(&s_force_fetch, true, __ATOMIC_RELEASE);
        kick();
    }
}

static void apply_playlist(void)
{
    if (s_pl.photo_count == 0) {
        abort_put();
        cancel_retry();

        if (!token_pending()) {
            /* 没有照片也没有未过期的 token：不初始化 SD。
             * 之前已经挂着的，顺手把不在清单里的缓存对账掉。 */
            if (photo_sd_mounted() && photo_sd_state() == PHOTO_READY) {
                photostore_t *store = photo_sd_store();
                if (store) {
                    gc_store(store);
                    update_stats(store);
                }
            }
            return;
        }

        ESP_LOGI(TAG, "清单暂无照片，但有未过期的 init token：挂卡检查状态");
        if (!mount_card_ready()) return;
        photostore_t *store = photo_sd_store();
        if (!store) {
            set_card(PHOTO_FAULT);
            return;
        }
        gc_store(store);
        update_stats(store);
        note_ready_transition();
        return;
    }

    if (!mount_card_ready()) return;

    photostore_t *store = photo_sd_store();
    if (!store) {
        abort_put();
        set_card(PHOTO_FAULT);
        return;
    }
    gc_store(store);

    if (s_put_live && !pl_find(&s_pl, s_put_sha)) abort_put();
    download_missing(store);
    update_stats(store);
    note_ready_transition();
}

static void reset_attempts(void)
{
    memset(s_fails, 0, sizeof s_fails);
    memset(s_next_try_us, 0, sizeof s_next_try_us);
}

static void run_sync(void)
{
    /* 开机只挂卡、不格式化：断网时也能从 SD 索引继续播已缓存照片。 */
    if (__atomic_exchange_n(&s_boot_mount, false, __ATOMIC_ACQ_REL)) {
        if (!photo_sd_mounted()) {
            if (photo_io_store_lock(30000)) {
                photo_sd_mount();
                photo_io_store_unlock();
            }
        }
        photo_state_t st = photo_sd_state();
        photo_state_t report = st;
        if (st == PHOTO_READY) {
            /* 索引里一张照片都没有：立刻卸载，别为无照片的卡常驻
             * sdmmc 驱动和 4KB 缓冲。有缓存照片才保持挂载供离线播放。 */
            if (photo_io_store_lock(30000)) {
                photostore_t *store = photo_sd_store();
                uint32_t bad = 0;
                int have = store ? photostore_walk(store, NULL, NULL, &bad) : 0;
                if (have == 0) {
                    photo_sd_unmount();
                    /* 状态仍报卡的真实状态（READY/UNINIT），只是没挂载：
                     * server 的 device show 不该把一张好卡显示成 absent。 */
                }
                photo_io_store_unlock();
            }
        } else if (st != PHOTO_ABSENT) {
            /* UNINIT/FAULT 的卡上没有可播的照片，先释放驱动和缓冲；
             * 等带 token/照片的清单到了再挂。 */
            photo_sd_unmount();
        }
        set_card(report);
        if (report == PHOTO_READY) update_stats(photo_sd_store());
        __atomic_add_fetch(&s_play_gen, 1, __ATOMIC_RELEASE);
        ESP_LOGI(TAG, "开机挂卡：state=%d 上报=%d", (int)st, (int)report);
    }

    /* MQTT 不在线：本轮不做 HTTPS（两次 TLS 同时握手会把堆打到几 KB）。
     * 不排重试——MQTT 连上时 cloud 会调 photo_sync_notify_connected()。
     * 离线照片照常从索引播。 */
    if (!cloud_connected()) {
        ESP_LOGI(TAG, "MQTT 未连接，本轮不拉清单/不下载");
        return;
    }

    bool force = __atomic_exchange_n(&s_force_fetch, false, __ATOMIC_ACQ_REL);
    int want = __atomic_load_n(&s_want_plv, __ATOMIC_ACQUIRE);
    bool need = force || !s_have_pl || s_last_plv < 0 ||
                (want >= 0 && want != s_last_plv);

    /* 维护窗口：照片播放让位给 HTTPS 和写卡，避免 TLS/播放/流发送叠加。 */
    bool paused = photo_play_pause(4000);
    if (!paused) {
        ESP_LOGW(TAG, "照片播放未能及时让出，2 秒后再同步");
        photo_play_resume();
        schedule_retry(esp_timer_get_time() + RETRY_SOON_US * 2);
        return;
    }
    if (need) {
        bool notmod = false;
        int r = fetch_playlist(&notmod);
        if (r == 0 && notmod) {
            if (want >= 0) s_last_plv = want;
            set_plv(s_last_plv);
            ESP_LOGI(TAG, "清单未变（304，plv=%d）", s_last_plv);
        } else if (r == 0) {
            s_last_plv = s_pl.plv;
            reset_attempts();
            prune_bad();
            set_plv(s_last_plv);
            /* 清单内容变了：只有这里（及下载/GC/格式化）通知播放器重建。 */
            __atomic_add_fetch(&s_play_gen, 1, __ATOMIC_RELEASE);
            ESP_LOGW(TAG, "清单更新 plv=%d，照片 %d 张", s_pl.plv,
                     s_pl.photo_count);
        } else if (r == -2) {
            ESP_LOGI(TAG, "服务端说还没配对（403），60 秒后再拉清单");
            photo_play_resume();
            schedule_retry(esp_timer_get_time() + FETCH_FAIL_US);
            return;
        } else if (r == -3) {
            /* 取帧优先/堆不足：1 秒后再试，不把这次算成清单失败。 */
            ESP_LOGI(TAG, "清单拉取延后（取帧优先或堆不足），1 秒后重试");
            photo_play_resume();
            schedule_retry(esp_timer_get_time() + RETRY_SOON_US);
            return;
        } else {
            ESP_LOGW(TAG, "清单拉取失败，60 秒后重试");
            schedule_retry(esp_timer_get_time() + FETCH_FAIL_US);
        }
    }

    if (s_have_pl) apply_playlist();
    /* 不再无条件 bump：304/失败/没下载的轮次不触发播放器重建。 */
    photo_play_resume();

    static bool reported;
    if (!reported) {
        reported = true;
        multi_heap_info_t in;
        heap_caps_get_info(&in, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
        ESP_LOGW(TAG, "heap 对账：free=%u largest=%u min=%u；静态 s_pl=%u "
                 "s_buf=%u retry=%u parser=%u",
                 (unsigned)in.total_free_bytes, (unsigned)in.largest_free_block,
                 (unsigned)in.minimum_free_bytes,
                 (unsigned)sizeof(s_pl), (unsigned)sizeof(s_buf),
                 (unsigned)(sizeof(s_fails) + sizeof(s_next_try_us)),
                 (unsigned)sizeof(s_pj));
    }
}

/* ------------------------------------------------------------ 任务 */

static void kick(void)
{
    if (__atomic_test_and_set(&s_running, __ATOMIC_SEQ_CST)) {
        __atomic_store_n(&s_kick, true, __ATOMIC_RELEASE);
        return;
    }
    __atomic_store_n(&s_kick, false, __ATOMIC_RELEASE);
    if (xTaskCreate(photo_task, "photo", TASK_STACK, NULL, TASK_PRIO, NULL) != pdPASS) {
        __atomic_clear(&s_running, __ATOMIC_SEQ_CST);
        ESP_LOGW(TAG, "照片任务创建失败，稍后重试");
        schedule_retry(esp_timer_get_time() + FETCH_FAIL_US);
    }
}

static void photo_task(void *arg)
{
    (void)arg;
    for (;;) {
        __atomic_store_n(&s_kick, false, __ATOMIC_RELEASE);
        run_sync();
        __atomic_clear(&s_running, __ATOMIC_SEQ_CST);
        if (!__atomic_load_n(&s_kick, __ATOMIC_ACQUIRE)) break;
        if (__atomic_test_and_set(&s_running, __ATOMIC_SEQ_CST)) break;
    }
    {
        multi_heap_info_t in;
        heap_caps_get_info(&in, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
        ESP_LOGI(TAG, "photo 任务退出：free=%u largest=%u min=%u",
                 (unsigned)in.total_free_bytes,
                 (unsigned)in.largest_free_block,
                 (unsigned)in.minimum_free_bytes);
    }
    vTaskDelete(NULL);
}

/* ------------------------------------------------------------ 对外 */

void photo_sync_init(void)
{
    photo_io_init();
    if (!s_pl_mux) s_pl_mux = xSemaphoreCreateMutex();

    memset(&s_snap, 0, sizeof s_snap);
    s_snap.state = photo_state_name(PHOTO_ABSENT);
    if (nvs_load_used(s_used_token, sizeof s_used_token)) {
        memcpy(s_snap.init_done, s_used_token, 8);
        s_snap.init_done[8] = 0;
        ESP_LOGI(TAG, "上次初始化用过的 token 前缀 %.8s", s_used_token);
    }

    const esp_timer_create_args_t args = {
        .callback = on_retry_timer,
        .name = "photo_retry",
    };
    if (esp_timer_create(&args, &s_retry_timer) != ESP_OK) {
        s_retry_timer = NULL;
        ESP_LOGW(TAG, "重试定时器创建失败，失败的照片本开机不再重试");
    }
    ESP_LOGI(TAG, "照片同步就绪（cap=true）");
}

void photo_sync_notify_plv(int plv)
{
    if (plv < 0) return;
    int prev = __atomic_exchange_n(&s_want_plv, plv, __ATOMIC_ACQ_REL);
    if (prev != plv || plv != __atomic_load_n(&s_last_plv, __ATOMIC_ACQUIRE))
        kick();
}

void photo_sync_notify_connected(void)
{
    __atomic_store_n(&s_force_fetch, true, __ATOMIC_RELEASE);
    kick();
}

void photo_sync_boot_mount(void)
{
    __atomic_store_n(&s_boot_mount, true, __ATOMIC_RELEASE);
    kick();
}

int photo_sync_status_json(char *out, size_t cap)
{
    if (!out || cap == 0) return -1;

    snap_t s;
    taskENTER_CRITICAL(&s_mux);
    s = s_snap;
    taskEXIT_CRITICAL(&s_mux);

    photo_status_t st = {
        .cap = true,
        .ready = s.ready,
        .plv = s.plv,
        .have = s.have,
        .free_kb = s.free_kb,
        .state = s.state,
        .init_done = s.init_done,
        .bad = &s.bad,
    };
    char obj[320];
    int n = photo_status_json(&st, obj, sizeof obj);
    if (n < 0) return -1;
    int k = snprintf(out, cap, "\"photo\":%s", obj);
    if (k < 0 || (size_t)k >= cap) return -1;
    return k;
}

/* ------------------------------------------------------------ M3 播放接口 */

uint32_t photo_sync_play_gen(void)
{
    return __atomic_load_n(&s_play_gen, __ATOMIC_ACQUIRE);
}

bool photo_sync_playlist_online(void)
{
    return __atomic_load_n(&s_have_pl, __ATOMIC_ACQUIRE);
}

bool photo_sync_playlist_lock(const photo_pl_t **pl, bool *online,
                              uint32_t timeout_ms)
{
    if (pl) *pl = NULL;
    if (online) *online = false;
    if (!s_pl_mux) {
        if (pl) *pl = &s_pl;
        if (online) *online = s_have_pl;
        return true;
    }
    TickType_t ticks = timeout_ms == UINT32_MAX ? portMAX_DELAY
                                                : pdMS_TO_TICKS(timeout_ms);
    if (xSemaphoreTake(s_pl_mux, ticks) != pdTRUE) return false;
    if (pl) *pl = &s_pl;
    if (online) *online = s_have_pl;
    return true;
}

void photo_sync_playlist_unlock(void)
{
    if (s_pl_mux) xSemaphoreGive(s_pl_mux);
}

typedef struct {
    photo_pl_t *pl;
} off_ctx_t;

static bool off_walk_cb(const photo_entry_t *e, void *arg)
{
    off_ctx_t *c = arg;
    if (c->pl->photo_count >= PHOTO_PL_MAX_PHOTOS) return false;
    photo_pl_entry_t *x = &c->pl->photos[c->pl->photo_count++];
    memcpy(x->sha256, e->sha256, 32);
    x->size = e->size;
    x->duration_s = e->duration_s;
    x->order = e->order;
    return true;
}

int photo_sync_build_offline(void)
{
    if (!s_pl_mux) return 0;
    xSemaphoreTake(s_pl_mux, portMAX_DELAY);
    if (s_have_pl) {   /* 拿到清单锁时在线清单赢了，别覆盖它 */
        int n = s_pl.photo_count;
        xSemaphoreGive(s_pl_mux);
        return n;
    }
    memset(&s_pl, 0, sizeof s_pl);
    if (photo_io_store_lock(1000)) {
        photostore_t *store = photo_sd_store();
        if (store) {
            off_ctx_t ctx = { .pl = &s_pl };
            uint32_t bad = 0;
            photostore_walk(store, off_walk_cb, &ctx, &bad);
        }
        photo_io_store_unlock();
    }
    int n = s_pl.photo_count;
    xSemaphoreGive(s_pl_mux);
    return n;
}

void photo_sync_report_bad(const uint8_t sha[32])
{
    char hex[65];
    sha_to_hex(sha, hex);
    bad_add(hex);
    ESP_LOGW(TAG, "播放读失败，标记 bad %.8s", hex);
}

bool photo_sync_formatting(void)
{
    return s_formatting;
}
