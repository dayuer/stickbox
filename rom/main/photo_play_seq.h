/*
 * 照片播放序列（M3）——纯逻辑，不依赖 ESP-IDF。
 *
 * **不复制清单。** 播放计划只是一组指向 photo_pl_t.photos 的下标：
 *   pos 0       board（虚拟项，时长取清单/默认 20s）
 *   pos 1..n    plan->ix[pos-1] 指向清单里的照片
 * 这样在线清单和播放序列共用同一份 8KB 数据，不再多占 8.8KB 静态 RAM。
 *
 * 时间用调用方注入的单调微秒。不可播的条目（文件缺失、bad、超单帧上限）
 * 整条跳过，不占停留时间。board 永远可播。
 */
#pragma once
#include <stdbool.h>
#include <stdint.h>

#include "photo_json.h"
#include "photostore.h"

#define PP_DEFAULT_BOARD_S 20
#define PP_DEFAULT_PHOTO_S 8

typedef enum {
    PP_BOARD = 0,
    PP_PHOTO = 1,
} pp_kind_t;

typedef struct {
    uint16_t ix[PHOTO_PL_MAX_PHOTOS];   /* 播放顺序：清单 photos[] 的下标 */
    uint16_t n;
    uint16_t board_s;
    bool     has_board;
} pp_plan_t;

typedef struct {
    uint8_t  kind;
    uint8_t  sha256[32];
    uint32_t size;
    uint16_t duration_s;
    uint16_t order;
} pp_item_t;

typedef bool (*pp_playable_cb)(const pp_item_t *it, void *arg);

typedef struct {
    int      pos;          /* 0=board, 1..n=照片 */
    int64_t  start_us;
    bool     has_photo;
    uint8_t  sha256[32];
    uint32_t size;
} pp_state_t;

void pp_plan_clear(pp_plan_t *p);
/* 在线清单：数组顺序权威，不排序。 */
int  pp_plan_from_playlist(pp_plan_t *p, const photo_pl_t *pl);
/* 离线索引：按 index 里的 order 稳定排序。 */
int  pp_plan_from_playlist_sorted(pp_plan_t *p, const photo_pl_t *pl);

/* 取 pos 处条目；board 不依赖 pl。pl 为 NULL 且 pos>0 返回 false。 */
bool pp_item_at(const photo_pl_t *pl, const pp_plan_t *p, int pos,
                pp_item_t *out);
uint32_t pp_duration_s(const pp_item_t *it);

void pp_reset(pp_state_t *st, const photo_pl_t *pl, const pp_plan_t *p,
              int64_t now_us, pp_playable_cb cb, void *arg);
bool pp_tick(pp_state_t *st, const photo_pl_t *pl, const pp_plan_t *p,
             int64_t now_us, pp_playable_cb cb, void *arg);
void pp_rebase(pp_state_t *st, const photo_pl_t *pl, const pp_plan_t *p,
               int64_t now_us, pp_playable_cb cb, void *arg);
bool pp_current(const pp_state_t *st, const photo_pl_t *pl,
                const pp_plan_t *p, pp_item_t *out);
