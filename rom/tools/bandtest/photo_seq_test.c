/*
 * 照片播放计划 host 测试（M3）：计划构建（在线数组顺序/离线 order 排序）、
 * 墙钟推进、跳过不可播项、计划重建保持当前照片，以及「无照片永远 board」
 * 回归。
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "photo_play_seq.h"

static int g_fails;

static void check(bool cond, const char *what)
{
    if (!cond) { printf("✗ %s\n", what); g_fails++; }
    else printf("✓ %s\n", what);
}

static void fill_sha(uint8_t sha[32], uint8_t v)
{
    memset(sha, v, 32);
}

static void add_photo(photo_pl_t *pl, uint8_t v, uint32_t size, uint16_t dur,
                      uint16_t order)
{
    photo_pl_entry_t *e = &pl->photos[pl->photo_count++];
    memset(e, 0, sizeof *e);
    fill_sha(e->sha256, v);
    e->size = size;
    e->duration_s = dur;
    e->order = order;
}

/* sha 全 0x22 = 照片 A，设为不可播。 */
static bool only_photos_ok(const pp_item_t *it, void *arg)
{
    (void)arg;
    if (it->kind == PP_BOARD) return true;
    return it->sha256[0] != 0x22;
}

static bool no_photos_ok(const pp_item_t *it, void *arg)
{
    (void)arg;
    return it->kind == PP_BOARD;
}

static void test_plan(void)
{
    photo_pl_t pl;
    memset(&pl, 0, sizeof pl);
    pl.board_seen = true;
    pl.board_duration_s = 20;
    add_photo(&pl, 0x22, 100, 8, 1);
    add_photo(&pl, 0x33, 200, 5, 2);

    pp_plan_t plan;
    check(pp_plan_from_playlist(&plan, &pl) == 2, "在线计划 2 张");
    pp_item_t it;
    check(pp_item_at(&pl, &plan, 0, &it) && it.kind == PP_BOARD &&
          pp_duration_s(&it) == 20, "pos0 = board 20s");
    check(pp_item_at(&pl, &plan, 1, &it) && it.kind == PP_PHOTO &&
          it.size == 100 && pp_duration_s(&it) == 8, "pos1 = 照片 A");
    check(pp_item_at(&pl, &plan, 2, &it) && it.size == 200 &&
          pp_duration_s(&it) == 5, "pos2 = 照片 B");

    /* 离线索引：order 乱序，duration 0 用默认 8s */
    photo_pl_t off;
    memset(&off, 0, sizeof off);
    add_photo(&off, 0x01, 10, 0, 5);
    add_photo(&off, 0x02, 20, 9, 2);
    add_photo(&off, 0x03, 30, 7, 9);
    pp_plan_from_playlist_sorted(&plan, &off);
    check(plan.n == 3, "离线计划 3 张");
    check(pp_item_at(&off, &plan, 1, &it) && it.order == 2, "离线按 order 排序 1");
    check(pp_item_at(&off, &plan, 2, &it) && it.order == 5, "离线按 order 排序 2");
    check(pp_item_at(&off, &plan, 3, &it) && it.order == 9, "离线按 order 排序 3");
    check(pp_duration_s(&it) == 7, "索引 duration");
    memset(&off, 0, sizeof off);
    add_photo(&off, 0x04, 40, 0, 0);
    pp_plan_from_playlist_sorted(&plan, &off);
    pp_item_at(&off, &plan, 1, &it);
    check(pp_duration_s(&it) == 8, "duration 0 用默认 8s");

    /* 空清单：只有 board */
    photo_pl_t empty;
    memset(&empty, 0, sizeof empty);
    check(pp_plan_from_playlist(&plan, &empty) == 0, "空清单计划 0 张");
    check(pp_item_at(&empty, &plan, 0, &it) && it.kind == PP_BOARD &&
          pp_duration_s(&it) == 20, "空清单只有 board");
}

static void test_tick(void)
{
    photo_pl_t pl;
    memset(&pl, 0, sizeof pl);
    pl.board_seen = true;
    pl.board_duration_s = 20;
    add_photo(&pl, 0x22, 100, 8, 1);
    add_photo(&pl, 0x33, 200, 5, 2);

    pp_plan_t plan;
    pp_plan_from_playlist(&plan, &pl);
    pp_state_t st;
    pp_reset(&st, &pl, &plan, 0, NULL, NULL);
    check(st.pos == 0 && !st.has_photo, "开机从 board 开始");
    check(!pp_tick(&st, &pl, &plan, 19900000, NULL, NULL), "19.9s 还在 board");
    check(pp_tick(&st, &pl, &plan, 20000000, NULL, NULL), "20s 切到照片 A");
    check(st.pos == 1 && st.has_photo && st.sha256[0] == 0x22, "当前照片 A");
    check(!pp_tick(&st, &pl, &plan, 27900000, NULL, NULL), "27.9s 还在 A");
    check(pp_tick(&st, &pl, &plan, 28000000, NULL, NULL), "28s 切到照片 B");
    check(st.pos == 2 && st.sha256[0] == 0x33, "当前照片 B");
    check(pp_tick(&st, &pl, &plan, 33000000, NULL, NULL), "33s 回到 board");
    check(st.pos == 0 && !st.has_photo, "回到 board");

    /* A 不可播：直接跳到 B */
    pp_reset(&st, &pl, &plan, 0, only_photos_ok, NULL);
    check(pp_tick(&st, &pl, &plan, 20000000, only_photos_ok, NULL) &&
          st.pos == 2, "A 不可播时直接跳到 B");

    /* 当前项失效：下一 tick 立刻换 */
    pp_reset(&st, &pl, &plan, 0, NULL, NULL);
    pp_tick(&st, &pl, &plan, 20000000, NULL, NULL);
    check(st.pos == 1, "先到 A");
    check(pp_tick(&st, &pl, &plan, 21000000, only_photos_ok, NULL) &&
          st.pos == 2, "A 中途失效 → 立刻跳到 B");

    /* 全部照片不可播：停在 board */
    pp_reset(&st, &pl, &plan, 0, no_photos_ok, NULL);
    for (int i = 0; i < 100; i++)
        pp_tick(&st, &pl, &plan, 20000000 + (int64_t)i * 1000000,
                no_photos_ok, NULL);
    check(st.pos == 0 && !st.has_photo, "没有可播照片时停在 board");
}

static void test_no_photo_regression(void)
{
    /* 需求 B：没有任何照片时，计划永远只有 board，一秒都不切走。 */
    photo_pl_t pl;
    memset(&pl, 0, sizeof pl);
    pp_plan_t plan;
    pp_plan_from_playlist(&plan, &pl);
    pp_state_t st;
    pp_reset(&st, &pl, &plan, 0, NULL, NULL);
    bool switched = false;
    for (int sec = 1; sec <= 3600; sec++)
        switched |= pp_tick(&st, &pl, &plan, (int64_t)sec * 1000000, NULL, NULL);
    check(!switched && st.pos == 0 && !st.has_photo,
          "无照片 1 小时内从不切出 board");
    pp_item_t it;
    check(pp_current(&st, &pl, &plan, &it) && it.kind == PP_BOARD,
          "无照片当前项恒为 board");
}

static void test_rebase(void)
{
    photo_pl_t pl;
    memset(&pl, 0, sizeof pl);
    add_photo(&pl, 0x22, 100, 8, 1);

    pp_plan_t plan;
    pp_plan_from_playlist(&plan, &pl);
    pp_state_t st;
    pp_reset(&st, &pl, &plan, 0, NULL, NULL);
    pp_tick(&st, &pl, &plan, 20000000, NULL, NULL);
    int64_t start = st.start_us;
    check(st.pos == 1 && start == 20000000, "已进入照片 A");

    /* 新计划仍含 A：保持位置和剩余时间 */
    pp_rebase(&st, &pl, &plan, 22000000, NULL, NULL);
    check(st.pos == 1 && st.start_us == start, "重建后保持当前照片与计时");

    /* 新计划不含 A：回 board，计时从现在开始 */
    photo_pl_t empty;
    memset(&empty, 0, sizeof empty);
    pp_plan_from_playlist(&plan, &empty);
    pp_rebase(&st, &empty, &plan, 23000000, NULL, NULL);
    check(st.pos == 0 && !st.has_photo && st.start_us == 23000000,
          "照片被删后重建回 board");

    /* board 正在播：重建保持 board 剩余时间 */
    st.start_us = 5000000;
    pp_plan_from_playlist(&plan, &pl);
    pp_rebase(&st, &pl, &plan, 6000000, NULL, NULL);
    check(st.pos == 0 && st.start_us == 5000000, "board 计时不被重建打断");
}

int main(void)
{
    test_plan();
    test_tick();
    test_no_photo_regression();
    test_rebase();
    if (g_fails) { printf("%d 项失败\n", g_fails); return 1; }
    printf("全部通过\n");
    return 0;
}
