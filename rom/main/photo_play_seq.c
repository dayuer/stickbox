#include "photo_play_seq.h"

#include <string.h>

static uint16_t norm_dur(uint8_t kind, uint16_t dur)
{
    if (kind == PP_BOARD) return dur ? dur : PP_DEFAULT_BOARD_S;
    return dur ? dur : PP_DEFAULT_PHOTO_S;
}

void pp_plan_clear(pp_plan_t *p)
{
    if (p) { memset(p, 0, sizeof *p); p->n = 0; }
}

int pp_plan_from_playlist(pp_plan_t *p, const photo_pl_t *pl)
{
    pp_plan_clear(p);
    if (!pl) return 0;
    p->board_s = pl->board_duration_s;
    p->has_board = pl->board_seen;
    p->n = (uint16_t)(pl->photo_count < PHOTO_PL_MAX_PHOTOS
                          ? pl->photo_count : PHOTO_PL_MAX_PHOTOS);
    for (int i = 0; i < p->n; i++) p->ix[i] = (uint16_t)i;
    return p->n;
}

int pp_plan_from_playlist_sorted(pp_plan_t *p, const photo_pl_t *pl)
{
    pp_plan_from_playlist(p, pl);
    /* 插入排序（≤200），按 order，稳定。 */
    for (int i = 1; i < p->n; i++) {
        uint16_t k = p->ix[i];
        int j = i - 1;
        while (j >= 0 && pl->photos[p->ix[j]].order > pl->photos[k].order) {
            p->ix[j + 1] = p->ix[j];
            j--;
        }
        p->ix[j + 1] = k;
    }
    return p->n;
}

bool pp_item_at(const photo_pl_t *pl, const pp_plan_t *p, int pos,
                pp_item_t *out)
{
    if (!p || !out || pos < 0 || pos > p->n) return false;
    memset(out, 0, sizeof *out);
    if (pos == 0) {
        out->kind = PP_BOARD;
        out->duration_s = norm_dur(PP_BOARD, p->board_s);
        return true;
    }
    if (!pl) return false;
    uint16_t ix = p->ix[pos - 1];
    if (ix >= pl->photo_count) return false;   /* 计划落后于新清单 */
    const photo_pl_entry_t *e = &pl->photos[ix];
    out->kind = PP_PHOTO;
    memcpy(out->sha256, e->sha256, 32);
    out->size = e->size;
    out->duration_s = norm_dur(PP_PHOTO, e->duration_s);
    out->order = e->order;
    return true;
}

uint32_t pp_duration_s(const pp_item_t *it)
{
    return norm_dur(it->kind, it->duration_s);
}

static bool playable_at(const photo_pl_t *pl, const pp_plan_t *p, int pos,
                        pp_playable_cb cb, void *arg)
{
    pp_item_t it;
    if (!pp_item_at(pl, p, pos, &it)) return false;
    return cb ? cb(&it, arg) : true;
}

static void set_current(pp_state_t *st, const pp_item_t *it)
{
    st->has_photo = it && it->kind == PP_PHOTO;
    if (st->has_photo) {
        memcpy(st->sha256, it->sha256, 32);
        st->size = it->size;
    } else {
        memset(st->sha256, 0, 32);
        st->size = 0;
    }
}

/* 从 pos+1 开始找下一个可播项（绕回），找不到返回 -1。 */
static int next_playable(const photo_pl_t *pl, const pp_plan_t *p, int pos,
                         pp_playable_cb cb, void *arg)
{
    if (!p || p->n + 1 <= 0) return -1;
    int count = p->n + 1;   /* board + photos */
    for (int k = 1; k <= count; k++) {
        int i = (pos + k) % count;
        if (i < 0) i += count;
        if (playable_at(pl, p, i, cb, arg)) return i;
    }
    return -1;
}

static void select_item(pp_state_t *st, const photo_pl_t *pl, const pp_plan_t *p,
                        int pos, int64_t now_us)
{
    st->pos = pos;
    st->start_us = now_us;
    pp_item_t it;
    if (pos >= 0 && pp_item_at(pl, p, pos, &it)) set_current(st, &it);
    else set_current(st, NULL);
}

void pp_reset(pp_state_t *st, const photo_pl_t *pl, const pp_plan_t *p,
              int64_t now_us, pp_playable_cb cb, void *arg)
{
    memset(st, 0, sizeof *st);
    st->pos = -1;
    if (!p) return;
    int pos = playable_at(pl, p, 0, cb, arg) ? 0
                                             : next_playable(pl, p, 0, cb, arg);
    select_item(st, pl, p, pos, now_us);
}

bool pp_tick(pp_state_t *st, const photo_pl_t *pl, const pp_plan_t *p,
             int64_t now_us, pp_playable_cb cb, void *arg)
{
    if (!p) return false;

    /* 当前项失效（照片被 GC/标 bad）：不等时长，立刻换。 */
    if (st->pos < 0 || st->pos > p->n ||
        !playable_at(pl, p, st->pos, cb, arg)) {
        int pos = next_playable(pl, p, st->pos, cb, arg);
        select_item(st, pl, p, pos, now_us);
        return true;
    }

    pp_item_t it;
    pp_item_at(pl, p, st->pos, &it);
    uint32_t dur = pp_duration_s(&it);
    if (now_us - st->start_us < (int64_t)dur * 1000000LL) return false;

    int pos = next_playable(pl, p, st->pos, cb, arg);
    if (pos < 0 || pos == st->pos) {
        /* 没有可播项，或只有当前这一项（board-only）：重新起算停留。 */
        st->start_us = now_us;
        return false;
    }
    select_item(st, pl, p, pos, now_us);
    return true;
}

void pp_rebase(pp_state_t *st, const photo_pl_t *pl, const pp_plan_t *p,
               int64_t now_us, pp_playable_cb cb, void *arg)
{
    if (!p) {
        select_item(st, pl, p, -1, now_us);
        return;
    }

    /* board 正在播：保持 board 的剩余时间，只换新计划。 */
    if (st->pos == 0 && !st->has_photo &&
        playable_at(pl, p, 0, cb, arg)) {
        select_item(st, pl, p, 0, st->start_us);
        return;
    }

    /* 当前照片仍在新计划且可播：保持位置和剩余时间。 */
    if (st->has_photo) {
        for (int i = 1; i <= p->n; i++) {
            pp_item_t it;
            if (!pp_item_at(pl, p, i, &it) || it.kind != PP_PHOTO) continue;
            if (memcmp(it.sha256, st->sha256, 32) == 0) {
                if (playable_at(pl, p, i, cb, arg)) {
                    st->pos = i;
                    set_current(st, &it);
                    return;
                }
                break;
            }
        }
    }

    /* 否则下一帧边界回 board。 */
    int pos = playable_at(pl, p, 0, cb, arg) ? 0
                                             : next_playable(pl, p, 0, cb, arg);
    select_item(st, pl, p, pos, now_us);
}

bool pp_current(const pp_state_t *st, const photo_pl_t *pl,
                const pp_plan_t *p, pp_item_t *out)
{
    if (!st || st->pos < 0) return false;
    return pp_item_at(pl, p, st->pos, out);
}
