#include "photo_play_seq.h"

#include <string.h>

static uint16_t norm_dur(uint8_t kind, uint16_t dur);

void pp_seq_clear(pp_seq_t *s)
{
    if (s) { memset(s, 0, sizeof *s); s->n = 0; }
}

static void add_item(pp_seq_t *s, uint8_t kind, const uint8_t sha[32],
                     uint32_t size, uint16_t dur, uint16_t order)
{
    if (s->n >= PP_MAX_ITEMS) return;
    pp_item_t *it = &s->items[s->n++];
    memset(it, 0, sizeof *it);
    it->kind = kind;
    if (sha) memcpy(it->sha256, sha, 32);
    it->size = size;
    it->duration_s = dur;
    it->order = order;
}

void pp_seq_begin(pp_seq_t *s, uint16_t board_s)
{
    pp_seq_clear(s);
    add_item(s, PP_BOARD, NULL, 0, norm_dur(PP_BOARD, board_s), 0);
}

void pp_seq_add_photo(pp_seq_t *s, const uint8_t sha[32], uint32_t size,
                      uint16_t dur, uint16_t order)
{
    add_item(s, PP_PHOTO, sha, size, norm_dur(PP_PHOTO, dur), order);
}

static uint16_t norm_dur(uint8_t kind, uint16_t dur)
{
    if (kind == PP_BOARD) return dur ? dur : PP_DEFAULT_BOARD_S;
    return dur ? dur : PP_DEFAULT_PHOTO_S;
}

int pp_seq_from_playlist(pp_seq_t *s, const photo_pl_t *pl)
{
    pp_seq_clear(s);
    if (!pl) return 0;
    /* board 永远在第一位；没有 board 条目时用默认时长。 */
    add_item(s, PP_BOARD, NULL, 0,
             norm_dur(PP_BOARD, pl->board_duration_s), 0);
    for (int i = 0; i < pl->photo_count; i++) {
        const photo_pl_entry_t *e = &pl->photos[i];
        add_item(s, PP_PHOTO, e->sha256, e->size,
                 norm_dur(PP_PHOTO, e->duration_s), e->order);
    }
    return s->n;
}

/* 照片按 order 稳定排序；board 固定在第一位。 */
void pp_seq_sort(pp_seq_t *s)
{
    for (int i = 2; i < s->n; i++) {
        pp_item_t k = s->items[i];
        int j = i - 1;
        while (j >= 1 && s->items[j].order > k.order) {
            s->items[j + 1] = s->items[j];
            j--;
        }
        s->items[j + 1] = k;
    }
}

int pp_seq_from_index(pp_seq_t *s, const photo_entry_t *e, int n,
                      uint16_t board_s)
{
    pp_seq_clear(s);
    add_item(s, PP_BOARD, NULL, 0, norm_dur(PP_BOARD, board_s), 0);
    for (int i = 0; i < n; i++) {
        add_item(s, PP_PHOTO, e[i].sha256, e[i].size,
                 norm_dur(PP_PHOTO, e[i].duration_s), e[i].order);
    }
    pp_seq_sort(s);
    return s->n;
}

uint32_t pp_duration_s(const pp_item_t *it)
{
    return norm_dur(it->kind, it->duration_s);
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

static bool playable(const pp_item_t *it, pp_playable_cb cb, void *arg)
{
    return cb ? cb(it, arg) : true;
}

/* 从 from+1 开始找下一个可播项（可绕回）。找不到返回 -1。 */
static int next_playable(const pp_seq_t *s, int from, pp_playable_cb cb,
                         void *arg)
{
    if (!s || s->n <= 0) return -1;
    for (int k = 1; k <= s->n; k++) {
        int i = (from + k) % s->n;
        if (i < 0) i += s->n;
        if (playable(&s->items[i], cb, arg)) return i;
    }
    return -1;
}

static void select_item(pp_state_t *st, const pp_seq_t *s, int pos,
                        int64_t now_us)
{
    st->pos = pos;
    st->start_us = now_us;
    if (pos >= 0) set_current(st, &s->items[pos]);
    else set_current(st, NULL);
}

void pp_reset(pp_state_t *st, const pp_seq_t *s, int64_t now_us,
              pp_playable_cb cb, void *arg)
{
    memset(st, 0, sizeof *st);
    st->pos = -1;
    if (!s || s->n <= 0) return;
    int pos = playable(&s->items[0], cb, arg) ? 0 : next_playable(s, 0, cb, arg);
    select_item(st, s, pos, now_us);
}

bool pp_tick(pp_state_t *st, const pp_seq_t *s, int64_t now_us,
             pp_playable_cb cb, void *arg)
{
    if (!s || s->n <= 0) return false;

    /* 当前项失效（照片被 GC/标 bad）：不等时长，立刻换。 */
    if (st->pos < 0 || st->pos >= s->n ||
        !playable(&s->items[st->pos], cb, arg)) {
        int pos = next_playable(s, st->pos, cb, arg);
        select_item(st, s, pos, now_us);
        return true;
    }

    uint32_t dur = pp_duration_s(&s->items[st->pos]);
    if (now_us - st->start_us < (int64_t)dur * 1000000LL) return false;

    int pos = next_playable(s, st->pos, cb, arg);
    if (pos < 0) {
        /* 没有可播项：停在 board（或当前项），重置停留计时，避免每 tick
         * 都重复判断。 */
        st->start_us = now_us;
        return false;
    }
    if (pos == st->pos) {
        /* 只有一个可播项（board-only 序列）：继续它，但重新起算停留，
         * 这不叫切换。 */
        st->start_us = now_us;
        return false;
    }
    select_item(st, s, pos, now_us);
    return true;
}

void pp_rebase(pp_state_t *st, const pp_seq_t *s, int64_t now_us,
               pp_playable_cb cb, void *arg)
{
    if (!s || s->n <= 0) {
        select_item(st, s, -1, now_us);
        return;
    }

    /* board 正在播：保持 board 的剩余时间，只换新序列。 */
    if (st->pos >= 0 && st->pos < s->n &&
        s->items[0].kind == PP_BOARD &&
        st->has_photo == false &&
        playable(&s->items[0], cb, arg)) {
        st->pos = 0;
        set_current(st, &s->items[0]);
        return;
    }

    /* 当前照片仍在新序列且可播：保持位置和剩余时间。 */
    if (st->has_photo) {
        for (int i = 1; i < s->n; i++) {
            if (s->items[i].kind == PP_PHOTO &&
                memcmp(s->items[i].sha256, st->sha256, 32) == 0) {
                if (playable(&s->items[i], cb, arg)) {
                    st->pos = i;
                    set_current(st, &s->items[i]);
                    return;
                }
                break;
            }
        }
    }

    /* 否则下一帧边界回 board。 */
    int pos = playable(&s->items[0], cb, arg) ? 0
                                              : next_playable(s, 0, cb, arg);
    select_item(st, s, pos, now_us);
}

bool pp_current(const pp_state_t *st, const pp_seq_t *s, pp_item_t *out)
{
    if (!st || !s || st->pos < 0 || st->pos >= s->n) return false;
    if (out) *out = s->items[st->pos];
    return true;
}
