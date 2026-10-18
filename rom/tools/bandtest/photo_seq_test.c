/*
 * 照片播放序列 host 测试（M3）：序列构建、墙钟推进、跳过不可播项、
 * 序列重建保持当前照片，以及「没有照片时永远是 board」回归。
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

/* 可播回调：把 photo A（sha 全 0x11）设为不可播时用它。 */
static bool only_photos_ok(const pp_item_t *it, void *arg)
{
    (void)arg;
    if (it->kind == PP_BOARD) return true;
    return it->sha256[0] != 0x22;   /* sha 全 0x22 = 照片 A */
}

static bool no_photos_ok(const pp_item_t *it, void *arg)
{
    (void)arg;
    return it->kind == PP_BOARD;
}

static void test_build(void)
{
    photo_pl_t pl;
    memset(&pl, 0, sizeof pl);
    pl.board_seen = true;
    pl.board_duration_s = 20;
    strcpy(pl.board_theme, "quota");
    pl.photo_count = 2;
    fill_sha(pl.photos[0].sha256, 0x22);
    pl.photos[0].size = 100;
    pl.photos[0].duration_s = 8;
    pl.photos[0].order = 1;
    fill_sha(pl.photos[1].sha256, 0x33);
    pl.photos[1].size = 200;
    pl.photos[1].duration_s = 5;
    pl.photos[1].order = 2;

    pp_seq_t s;
    int n = pp_seq_from_playlist(&s, &pl);
    check(n == 3, "清单序列 board + 2 照片");
    check(s.items[0].kind == PP_BOARD && pp_duration_s(&s.items[0]) == 20,
          "board 20s 在第一位");
    check(s.items[1].kind == PP_PHOTO && s.items[1].size == 100 &&
          pp_duration_s(&s.items[1]) == 8, "照片 A");
    check(s.items[2].kind == PP_PHOTO && s.items[2].size == 200 &&
          pp_duration_s(&s.items[2]) == 5, "照片 B");

    /* 索引：order 乱序，duration 0 用默认 8s */
    photo_entry_t e[3];
    memset(e, 0, sizeof e);
    fill_sha(e[0].sha256, 0x01); e[0].size = 10; e[0].duration_s = 0; e[0].order = 5;
    fill_sha(e[1].sha256, 0x02); e[1].size = 20; e[1].duration_s = 9; e[1].order = 2;
    fill_sha(e[2].sha256, 0x03); e[2].size = 30; e[2].duration_s = 7; e[2].order = 9;
    n = pp_seq_from_index(&s, e, 3, 0);
    check(n == 4 && s.items[0].kind == PP_BOARD &&
          pp_duration_s(&s.items[0]) == 20, "索引序列 board 默认 20s");
    check(s.items[1].order == 2 && s.items[2].order == 5 &&
          s.items[3].order == 9, "索引按 order 稳定排序");
    check(pp_duration_s(&s.items[2]) == 8, "索引 duration=0 用默认 8s");

    /* 没有照片：只有 board */
    n = pp_seq_from_playlist(&s, NULL);
    check(n == 0, "空清单长度为 0");

    photo_pl_t empty;
    memset(&empty, 0, sizeof empty);
    n = pp_seq_from_playlist(&s, &empty);
    check(n == 1 && s.items[0].kind == PP_BOARD, "空清单 = 只有 board");
}

static void test_tick(void)
{
    photo_pl_t pl;
    memset(&pl, 0, sizeof pl);
    pl.photo_count = 2;
    fill_sha(pl.photos[0].sha256, 0x22);
    pl.photos[0].size = 100; pl.photos[0].duration_s = 8; pl.photos[0].order = 1;
    fill_sha(pl.photos[1].sha256, 0x33);
    pl.photos[1].size = 200; pl.photos[1].duration_s = 5; pl.photos[1].order = 2;

    pp_seq_t s;
    pp_seq_from_playlist(&s, &pl);
    pp_state_t st;
    pp_reset(&st, &s, 0, NULL, NULL);
    check(st.pos == 0 && !st.has_photo, "开机从 board 开始");
    check(!pp_tick(&st, &s, 19900000, NULL, NULL), "19.9s 还在 board");
    check(pp_tick(&st, &s, 20000000, NULL, NULL), "20s 切到照片 A");
    check(st.pos == 1 && st.has_photo && st.sha256[0] == 0x22, "当前照片 A");
    check(!pp_tick(&st, &s, 27900000, NULL, NULL), "27.9s 还在 A");
    check(pp_tick(&st, &s, 28000000, NULL, NULL), "28s 切到照片 B");
    check(st.pos == 2 && st.sha256[0] == 0x33, "当前照片 B");
    check(pp_tick(&st, &s, 33000000, NULL, NULL), "33s 回到 board");
    check(st.pos == 0 && !st.has_photo, "回到 board");

    /* 跳过不可播的 A */
    pp_reset(&st, &s, 0, only_photos_ok, NULL);
    check(pp_tick(&st, &s, 20000000, only_photos_ok, NULL) &&
          st.pos == 2, "A 不可播时直接跳到 B");

    /* 当前项失效：不等时长，下一 tick 立刻换 */
    pp_reset(&st, &s, 0, NULL, NULL);
    pp_tick(&st, &s, 20000000, NULL, NULL);      /* A */
    check(st.pos == 1, "先到 A");
    check(pp_tick(&st, &s, 21000000, only_photos_ok, NULL) && st.pos == 2,
          "A 中途失效 → 立刻跳到 B");

    /* 全部照片不可播：停在 board */
    pp_reset(&st, &s, 0, no_photos_ok, NULL);
    for (int i = 0; i < 100; i++)
        pp_tick(&st, &s, 20000000 + (int64_t)i * 1000000, no_photos_ok, NULL);
    check(st.pos == 0 && !st.has_photo, "没有可播照片时停在 board");
}

static void test_no_photo_regression(void)
{
    /* 需求 B：没有任何照片时，序列永远只有 board，一秒都不切走。 */
    photo_pl_t pl;
    memset(&pl, 0, sizeof pl);
    pp_seq_t s;
    pp_seq_from_playlist(&s, &pl);
    pp_state_t st;
    pp_reset(&st, &s, 0, NULL, NULL);
    bool switched = false;
    for (int sec = 1; sec <= 3600; sec++)
        switched |= pp_tick(&st, &s, (int64_t)sec * 1000000, NULL, NULL);
    check(!switched && st.pos == 0 && !st.has_photo,
          "无照片 1 小时内从不切出 board");
    pp_item_t it;
    check(pp_current(&st, &s, &it) && it.kind == PP_BOARD,
          "无照片当前项恒为 board");
}

static void test_rebase(void)
{
    photo_pl_t pl;
    memset(&pl, 0, sizeof pl);
    pl.photo_count = 1;
    fill_sha(pl.photos[0].sha256, 0x22);
    pl.photos[0].size = 100; pl.photos[0].duration_s = 8; pl.photos[0].order = 1;

    pp_seq_t s;
    pp_seq_from_playlist(&s, &pl);
    pp_state_t st;
    pp_reset(&st, &s, 0, NULL, NULL);
    pp_tick(&st, &s, 20000000, NULL, NULL);
    int64_t start = st.start_us;
    check(st.pos == 1 && start == 20000000, "已进入照片 A");

    /* 新序列仍含 A：保持位置和剩余时间 */
    pp_rebase(&st, &s, 22000000, NULL, NULL);
    check(st.pos == 1 && st.start_us == start, "重建后保持当前照片与计时");

    /* 新序列不含 A：回 board，计时从现在开始 */
    photo_pl_t empty;
    memset(&empty, 0, sizeof empty);
    pp_seq_t s2;
    pp_seq_from_playlist(&s2, &empty);
    pp_rebase(&st, &s2, 23000000, NULL, NULL);
    check(st.pos == 0 && !st.has_photo && st.start_us == 23000000,
          "照片被删后重建回 board");

    /* board 正在播：重建保持 board 剩余时间 */
    st.start_us = 5000000;
    pp_rebase(&st, &s, 6000000, NULL, NULL);
    check(st.pos == 0 && st.start_us == 5000000, "board 计时不被重建打断");
}

int main(void)
{
    test_build();
    test_tick();
    test_no_photo_regression();
    test_rebase();
    if (g_fails) { printf("%d 项失败\n", g_fails); return 1; }
    printf("全部通过\n");
    return 0;
}
