/*
 * 照片播放调度（M3）。
 *
 * 序列和计时在 photo_play_seq.c（纯逻辑，host 可测）；这里只做三件事：
 *   1. 按 photo_sync 的代数重建序列（在线清单 / 离线索引）；
 *   2. 100ms tick 推进当前项，发布一个很小的「当前照片」快照；
 *   3. 给 stream.c 的连接任务提供帧快照 + hold 计数，防止 GC 在发送
 *      途中把正在读的数据区回收掉。
 *
 * 所有 photostore 访问都在 store 锁里；本文件不碰 net 锁，所以照片播放
 * 与取帧 TLS 可以并行。
 */
#include "photo_play.h"

#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "esp_log.h"
#include "esp_timer.h"

#include "cloud_client.h"
#include "framepack.h"
#include "photo_io.h"
#include "photo_play_seq.h"
#include "photo_sd.h"
#include "photo_sync.h"
#include "photostore.h"
#include "stream.h"

static const char *TAG = "photoplay";

#define PLAY_TICK_MS 100
#define PLAY_STACK   4096
#define PLAY_PRIO    2

/* AVI 头里报的单帧上限（与 stream.c 的 build_header 同一个公式）。
 * 服务端照片 ≤250KB，正常的都能过；超限的条目不播。 */
#define PHOTO_PLAY_MAX_FRAME (FP_MAX_PACK + BAND_CAP + 8)

/* 播放计划只存下标，照片本体在 photo_sync 的 s_pl 里（省 8.8KB 静态 RAM）。 */
static pp_plan_t  s_plan;
static pp_state_t s_work;

typedef struct {
    bool     has_photo;
    uint8_t  sha256[32];
    uint32_t size;
} pub_t;
static pub_t s_pub;

typedef struct {
    uint8_t sha256[32];
    uint8_t n;
} hold_t;
static hold_t s_holds[8];

static uint8_t  s_bad[8][32];
static int      s_bad_n;
static uint8_t  s_fail_sha[32];
static int      s_fail_count;
static volatile bool s_force_rebuild;
static volatile bool s_pause_req;
/* 暂停是引用计数：照片同步和崩溃镜像上传都会开维护窗口，各自配对
 * pause/resume。单个布尔量会让先结束的那一方把另一方的暂停解掉。 */
static int s_pause_refs;
static volatile bool s_paused;
static int64_t       s_pause_t0;

static portMUX_TYPE s_mux = portMUX_INITIALIZER_UNLOCKED;

/* 临界区内的辅助函数（调用方已持 s_mux）。 */
static bool bad_has_locked(const uint8_t sha[32])
{
    for (int i = 0; i < s_bad_n; i++)
        if (memcmp(s_bad[i], sha, 32) == 0) return true;
    return false;
}

static bool hold_add_locked(const uint8_t sha[32])
{
    int free_i = -1;
    for (int i = 0; i < (int)(sizeof s_holds / sizeof s_holds[0]); i++) {
        if (s_holds[i].n && memcmp(s_holds[i].sha256, sha, 32) == 0) {
            if (s_holds[i].n < 255) s_holds[i].n++;
            return true;
        }
        if (!s_holds[i].n && free_i < 0) free_i = i;
    }
    if (free_i < 0) return false;
    memcpy(s_holds[free_i].sha256, sha, 32);
    s_holds[free_i].n = 1;
    return true;
}

static void hold_del_locked(const uint8_t sha[32])
{
    for (int i = 0; i < (int)(sizeof s_holds / sizeof s_holds[0]); i++) {
        if (s_holds[i].n && memcmp(s_holds[i].sha256, sha, 32) == 0) {
            if (--s_holds[i].n == 0) memset(&s_holds[i], 0, sizeof s_holds[i]);
            return;
        }
    }
}

/* ------------------------------------------------------------ 快照 */

static void publish(void)
{
    bool allow = !(s_paused || s_pause_req);
    taskENTER_CRITICAL(&s_mux);
    s_pub.has_photo = allow && s_work.has_photo;
    memcpy(s_pub.sha256, s_work.sha256, 32);
    s_pub.size = s_work.size;
    taskEXIT_CRITICAL(&s_mux);
}

/* ------------------------------------------------------------ 可播性 */

static bool playable_cb(const pp_item_t *it, void *arg)
{
    (void)arg;
    if (it->kind == PP_BOARD) return true;
    if (photo_sync_formatting()) return false;
    if (it->size == 0 || it->size + 8 > PHOTO_PLAY_MAX_FRAME) return false;

    bool bad;
    taskENTER_CRITICAL(&s_mux);
    bad = bad_has_locked(it->sha256);
    taskEXIT_CRITICAL(&s_mux);
    if (bad) return false;

    photostore_t *store = photo_sd_store();
    if (!store) return false;
    if (!photo_io_store_lock(1000)) return true;   /* 暂时拿不到锁，不判死 */
    photo_file_t f;
    bool ok = photostore_get(store, it->sha256, &f) && f.size == it->size;
    photo_io_store_unlock();
    return ok;
}

/* ------------------------------------------------------------ 重建 */

static void rebuild(void)
{
    if (!photo_sync_playlist_online()) photo_sync_build_offline();

    const photo_pl_t *pl = NULL;
    bool online = false;
    bool locked = photo_sync_playlist_lock(&pl, &online, 2000);
    if (locked) {
        if (online) pp_plan_from_playlist(&s_plan, pl);
        else pp_plan_from_playlist_sorted(&s_plan, pl);
    } else {
        pp_plan_clear(&s_plan);   /* 拿不到清单锁：这轮只播 board */
    }

    pp_rebase(&s_work, pl, &s_plan, esp_timer_get_time(), playable_cb, NULL);
    publish();
    if (locked) photo_sync_playlist_unlock();

    ESP_LOGI(TAG, "序列重建：照片 %d 张（%s），当前=%s", s_plan.n,
             online ? "在线" : "索引", s_work.has_photo ? "照片" : "board");
}

/* ------------------------------------------------------------ 任务 */

static void play_task(void *arg)
{
    (void)arg;
    uint32_t gen = 0;
    bool built = false;

    for (;;) {
        /* 维护窗口：切回 board，冻结当前项的计时，等网络工作做完。 */
        if (s_pause_req) {
            if (!s_paused) {
                s_paused = true;
                s_pause_t0 = esp_timer_get_time();
                publish();   /* publish() 看到 paused，会强制 board */
                ESP_LOGI(TAG, "照片播放暂停，让出给清单/下载");
            }
        } else if (s_paused) {
            s_paused = false;
            int64_t now = esp_timer_get_time();
            /* 只冻结暂停前那一项的计时；暂停期间重建过（start_us 已是
             * 新起点）就不要再加一遍。 */
            if (s_work.start_us < s_pause_t0)
                s_work.start_us += now - s_pause_t0;
            publish();
            ESP_LOGI(TAG, "照片播放恢复");
        }

        uint32_t g = photo_sync_play_gen();
        if (!built || g != gen || s_force_rebuild) {
            s_force_rebuild = false;
            gen = g;
            built = true;
            rebuild();
        }
        if (!s_paused) {
            const photo_pl_t *pl = NULL;
            bool online = false;
            if (photo_sync_playlist_lock(&pl, &online, 100)) {
                if (pp_tick(&s_work, pl, &s_plan, esp_timer_get_time(),
                            playable_cb, NULL))
                    publish();
                photo_sync_playlist_unlock();
            }
        }
        vTaskDelay(pdMS_TO_TICKS(PLAY_TICK_MS));
    }
}

/* ------------------------------------------------------------ 对外 */

void photo_play_start(void)
{
    if (xTaskCreate(play_task, "photo_play", PLAY_STACK, NULL, PLAY_PRIO, NULL)
        != pdPASS) {
        ESP_LOGE(TAG, "播放调度任务创建失败，照片不播，看板照常");
    }
}

void photo_play_notify_playlist(void)
{
    s_force_rebuild = true;
}

bool photo_play_pause(uint32_t max_wait_ms)
{
    __atomic_add_fetch(&s_pause_refs, 1, __ATOMIC_ACQ_REL);
    s_pause_req = true;
    int64_t t0 = esp_timer_get_time();
    while (esp_timer_get_time() - t0 < (int64_t)max_wait_ms * 1000) {
        if (s_paused && photo_play_holds() == 0) {
            /* 再等一拍，让 stream 的下一个 board 帧把读缓冲释放掉。
             * 1200ms 覆盖 1fps（帧间隔 1s）。 */
            vTaskDelay(pdMS_TO_TICKS(1200));
            return true;
        }
        vTaskDelay(pdMS_TO_TICKS(20));
    }
    return false;
}

void photo_play_resume(void)
{
    int left = __atomic_sub_fetch(&s_pause_refs, 1, __ATOMIC_ACQ_REL);
    if (left < 0) {
        /* 多调了一次 resume：归零，不让计数变负把后面的 pause 吞掉。 */
        __atomic_store_n(&s_pause_refs, 0, __ATOMIC_RELEASE);
        left = 0;
    }
    if (left == 0) s_pause_req = false;
}

bool photo_play_frame(photo_play_frame_t *out)
{
    if (!out) return false;
    if (s_pause_req || s_paused) return false;
    if (cloud_pair_pending() || photo_sync_formatting() ||
        photo_sd_state() != PHOTO_READY) return false;

    pub_t p;
    taskENTER_CRITICAL(&s_mux);
    p = s_pub;
    taskEXIT_CRITICAL(&s_mux);
    if (!p.has_photo || p.size == 0 || p.size + 8 > PHOTO_PLAY_MAX_FRAME)
        return false;

    photostore_t *store = photo_sd_store();
    if (!store) return false;
    if (!photo_io_store_lock(1000)) return false;

    photo_file_t f;
    bool ok = photostore_get(store, p.sha256, &f) && f.size == p.size;
    if (ok) {
        /* 在 store 锁里登记 hold：GC 也拿同一把锁，不可能在这中间把
         * 索引项回收掉。 */
        taskENTER_CRITICAL(&s_mux);
        if (!hold_add_locked(p.sha256)) ok = false;
        taskEXIT_CRITICAL(&s_mux);
    }
    photo_io_store_unlock();
    if (!ok) return false;

    out->active = true;
    memcpy(out->sha256, p.sha256, 32);
    out->size = f.size;
    out->start_sector = f.start_sector;
    return true;
}

void photo_play_frame_done(const photo_play_frame_t *pf)
{
    if (!pf || !pf->active) return;
    taskENTER_CRITICAL(&s_mux);
    hold_del_locked(pf->sha256);
    taskEXIT_CRITICAL(&s_mux);
}

bool photo_play_is_held(const uint8_t sha[32])
{
    bool held = false;
    taskENTER_CRITICAL(&s_mux);
    for (int i = 0; i < (int)(sizeof s_holds / sizeof s_holds[0]); i++)
        if (s_holds[i].n && memcmp(s_holds[i].sha256, sha, 32) == 0) {
            held = true;
            break;
        }
    taskEXIT_CRITICAL(&s_mux);
    return held;
}

int photo_play_holds(void)
{
    int total = 0;
    taskENTER_CRITICAL(&s_mux);
    for (int i = 0; i < (int)(sizeof s_holds / sizeof s_holds[0]); i++)
        total += s_holds[i].n;
    taskEXIT_CRITICAL(&s_mux);
    return total;
}

void photo_play_frame_ok(const uint8_t sha[32])
{
    taskENTER_CRITICAL(&s_mux);
    if (s_fail_count && memcmp(s_fail_sha, sha, 32) == 0) s_fail_count = 0;
    taskEXIT_CRITICAL(&s_mux);
}

void photo_play_mark_bad(const uint8_t sha[32], const char *why)
{
    bool report = false;
    taskENTER_CRITICAL(&s_mux);
    if (s_fail_count && memcmp(s_fail_sha, sha, 32) == 0) {
        s_fail_count++;
    } else {
        memcpy(s_fail_sha, sha, 32);
        s_fail_count = 1;
    }
    if (s_fail_count >= 3) {
        s_fail_count = 0;
        if (!bad_has_locked(sha) && s_bad_n < (int)(sizeof s_bad / sizeof s_bad[0])) {
            memcpy(s_bad[s_bad_n++], sha, 32);
            report = true;
        }
    }
    taskEXIT_CRITICAL(&s_mux);

    if (report) {
        ESP_LOGW(TAG, "照片连续 3 次读失败，标记 bad（%s）", why ? why : "");
        photo_sync_report_bad(sha);   /* 进 status.photo.bad，并阻止再下载 */
    }
}

bool photo_is_playing(void)
{
    if (s_pause_req || s_paused) return false;
    if (cloud_pair_pending() || photo_sync_formatting() ||
        photo_sd_state() != PHOTO_READY) return false;
    bool p;
    taskENTER_CRITICAL(&s_mux);
    p = s_pub.has_photo;
    taskEXIT_CRITICAL(&s_mux);
    return p;
}
