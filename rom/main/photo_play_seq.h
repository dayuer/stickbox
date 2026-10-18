/*
 * 照片播放序列（M3）——纯逻辑，不依赖 ESP-IDF。
 *
 * 序列来自两处：
 *   - 在线：M2 拉到的清单（board + photos，数组顺序权威）
 *   - 离线：SD 索引（board 默认 20s，照片按 order 排序）
 *
 * 时间用调用方注入的单调微秒（板上是 esp_timer_get_time()，host 测试是假
 * 时钟）。调度只做「当前项/下一项」的决定；真正的发送在 stream.c 的帧
 * 边界。切换因此最多晚一个帧间隔。
 *
 * 不可播的条目（文件缺失、下载失败、超过 AVI 单帧上限）整条跳过，不占
 * 停留时间。board 永远可播（最差是预置/状态页）。
 */
#pragma once
#include <stdbool.h>
#include <stdint.h>

#include "photo_json.h"
#include "photostore.h"

#define PP_MAX_ITEMS       (PHOTO_PL_MAX_PHOTOS + 1)
#define PP_DEFAULT_BOARD_S 20
#define PP_DEFAULT_PHOTO_S 8

typedef enum {
    PP_BOARD = 0,
    PP_PHOTO = 1,
} pp_kind_t;

typedef struct {
    uint8_t  kind;
    uint8_t  sha256[32];
    uint32_t size;
    uint16_t duration_s;
    uint16_t order;
} pp_item_t;

typedef struct {
    pp_item_t items[PP_MAX_ITEMS];
    int       n;
} pp_seq_t;

/* 当前项是否可播。board 由回调返回 true；照片检查索引/大小/本地 bad。 */
typedef bool (*pp_playable_cb)(const pp_item_t *it, void *arg);

typedef struct {
    int      pos;
    int64_t  start_us;
    bool     has_photo;
    uint8_t  sha256[32];
    uint32_t size;
} pp_state_t;

void pp_seq_clear(pp_seq_t *s);
/* 手工构建（播放器从清单逐项 visit / 从索引 walk 时用）。 */
void pp_seq_begin(pp_seq_t *s, uint16_t board_s);
void pp_seq_add_photo(pp_seq_t *s, const uint8_t sha[32], uint32_t size,
                      uint16_t dur, uint16_t order);
void pp_seq_sort(pp_seq_t *s);

int  pp_seq_from_playlist(pp_seq_t *s, const photo_pl_t *pl);
int  pp_seq_from_index(pp_seq_t *s, const photo_entry_t *e, int n,
                       uint16_t board_s);

void pp_reset(pp_state_t *st, const pp_seq_t *s, int64_t now_us,
              pp_playable_cb cb, void *arg);
/* 到点/当前项失效则推进；返回是否换了项。 */
bool pp_tick(pp_state_t *st, const pp_seq_t *s, int64_t now_us,
             pp_playable_cb cb, void *arg);
/* 序列重建后调用：当前照片仍在新序列且可播就保持剩余时间，否则从
 * 第一项（board）重新开始。 */
void pp_rebase(pp_state_t *st, const pp_seq_t *s, int64_t now_us,
               pp_playable_cb cb, void *arg);

bool pp_current(const pp_state_t *st, const pp_seq_t *s, pp_item_t *out);
uint32_t pp_duration_s(const pp_item_t *it);
