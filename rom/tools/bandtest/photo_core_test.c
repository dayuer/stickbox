/*
 * 照片链路纯逻辑 host 测试（M2）：
 *   - 流式清单解析（跨 chunk、未知 kind、嵌套跳过、容量上限、拒绝路径）
 *   - 初始化 token 的格式化判决（BLANK/RESUME/CORRUPT/READY、过期、未同步）
 *   - 退避、失败前缀去重、status.photo JSON 与 512B 预算
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "photo_json.h"
#include "photo_policy.h"

static int g_fails;

static void check(bool cond, const char *what)
{
    if (!cond) { printf("✗ %s\n", what); g_fails++; }
    else printf("✓ %s\n", what);
}

static const char *DOC =
    "{\n"
    "  \"plv\": 7,\n"
    "  \"init_token\": \"3f2a9c01d4e5b6a7\",\n"
    "  \"init_force\": false,\n"
    "  \"init_expires_at\": 1760000000,\n"
    "  \"entries\": [\n"
    "    {\"kind\":\"board\",\"theme\":\"quota\",\"ver\":12,\"duration_s\":20,\"order\":0},\n"
    "    {\"kind\":\"photo\",\"sha256\":\"3f2a9c01d4e5b6a7c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4de\",\n"
    "     \"size\":198432,\"duration_s\":8,\"order\":1,\"url\":\"/api/device/photo/x\"},\n"
    "    {\"kind\":\"photo\",\"sha256\":\"9c01d4e5b6a7c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f0\",\n"
    "     \"size\":241760,\"duration_s\":8,\"order\":2}\n"
    "  ]\n"
    "}\n";

static bool parse_chunks(const char *s, size_t len, size_t step, photo_pl_t *pl)
{
    photo_json_t pj;
    photo_json_init(&pj, pl);
    size_t off = 0;
    while (off < len) {
        size_t n = step;
        if (n > len - off) n = len - off;
        if (!photo_json_feed(&pj, s + off, n)) return false;
        off += n;
    }
    return photo_json_finish(&pj) && pl->has_plv && pl->entries_seen;
}

static void test_json(void)
{
    photo_pl_t pl;
    size_t len = strlen(DOC);

    check(parse_chunks(DOC, len, len, &pl), "整块解析");
    check(pl.has_plv && pl.plv == 7, "plv=7");
    check(pl.has_token && strcmp(pl.init_token, "3f2a9c01d4e5b6a7") == 0, "init_token");
    check(!pl.init_force, "init_force=false");
    check(pl.has_expires && pl.expires_at == 1760000000LL, "init_expires_at");
    check(pl.entries_seen && pl.entry_count == 3, "entries 3 条");
    check(pl.photo_count == 2, "只收 photo 条目（board 忽略）");
    check(pl.photos[0].size == 198432 && pl.photos[1].size == 241760, "size");
    check(pl.photos[0].order == 1 && pl.photos[1].order == 2, "order");
    check(pl.photos[0].duration_s == 8, "duration_s");

    check(parse_chunks(DOC, len, 1, &pl), "逐字节喂");
    check(pl.photo_count == 2 && pl.photos[0].order == 1, "逐字节结果一致");

    check(parse_chunks(DOC, len, 7, &pl), "7 字节分片");
    check(pl.photo_count == 2, "7 字节分片结果一致");

    /* 字符串转义 + 未知嵌套对象/数组 */
    static const char *X =
        "{\"plv\":3,\"entries\":[{\"kind\":\"photo\",\"sha256\":\""
        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        "\",\"size\":10,\"order\":0,\"weight\":2.5,"
        "\"junk\":{\"a\":[1,2,{\"b\":\"x\\\"y\\u4e2d\"}]},"
        "\"kind2\":\"photo\"}],\"note\":\"\\u4e2d\\n\",\"ratio\":1.5}";
    photo_json_t pj;
    photo_json_init(&pj, &pl);
    check(photo_json_feed(&pj, X, strlen(X)) && photo_json_finish(&pj),
          "转义与未知嵌套可跳过");
    check(pl.photo_count == 1 && pl.photos[0].size == 10, "嵌套后的 photo 条目");

    /* 未知 kind 整条忽略 */
    static const char *U =
        "{\"plv\":1,\"entries\":[{\"kind\":\"video\",\"sha256\":\"zz\",\"size\":1}]}";
    check(parse_chunks(U, strlen(U), 3, &pl) && pl.photo_count == 0,
          "未知 kind 忽略");

    /* 200 张上限 */
    size_t cap = 200 * 140 + 256;
    char *big = malloc(cap);
    size_t o = 0;
    o += (size_t)snprintf(big + o, cap - o, "{\"plv\":9,\"entries\":[");
    for (int i = 0; i < 200; i++) {
        char sha[65];
        for (int k = 0; k < 64; k++) sha[k] = "0123456789abcdef"[k % 16];
        sha[i % 64] = "0123456789abcdef"[(i + 1) % 16];
        sha[64] = 0;
        o += (size_t)snprintf(big + o, cap - o,
                              "%s{\"kind\":\"photo\",\"sha256\":\"%s\",\"size\":1024,"
                              "\"duration_s\":8,\"order\":%d}",
                              i ? "," : "", sha, i);
    }
    snprintf(big + o, cap - o, "]}");
    check(parse_chunks(big, strlen(big), 4096, &pl) && pl.photo_count == 200,
          "200 张照片");

    /* 201 张必须整份拒绝 */
    o = 0;
    o += (size_t)snprintf(big + o, cap - o, "{\"plv\":9,\"entries\":[");
    for (int i = 0; i < 201; i++) {
        o += (size_t)snprintf(big + o, cap - o,
                              "%s{\"kind\":\"photo\",\"sha256\":\"%064d\","
                              "\"size\":1024,\"duration_s\":8,\"order\":%d}",
                              i ? "," : "", i % 10, i);
    }
    snprintf(big + o, cap - o, "]}");
    check(!parse_chunks(big, strlen(big), 4096, &pl), "超 200 张整份拒绝");
    free(big);

    /* 拒绝路径 */
    static const char *BAD[] = {
        "{\"plv\":1,\"entries\":[",
        "{\"plv\":\"7\",\"entries\":[]}",
        "{\"plv\":1,\"entries\":[{\"kind\":\"photo\",\"sha256\":\"GG\",\"size\":1}]}",
        "{\"plv\":1,\"entries\":[{\"kind\":\"photo\",\"size\":1}]}",
        "{\"plv\":1,\"entries\":[{\"kind\":\"photo\",\"sha256\":"
        "\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\","
        "\"size\":0}]}",
        "{\"plv\":1,\"entries\":[{\"kind\":\"photo\",\"sha256\":"
        "\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\","
        "\"size\":1.5}]}",
        "{\"plv\":1,\"entries\":[]} trailing",
        "{\"plv\":1,\"entries\":[}",
        "{\"plv\":1}",
    };
    for (size_t i = 0; i < sizeof BAD / sizeof BAD[0]; i++)
        check(!parse_chunks(BAD[i], strlen(BAD[i]), 5, &pl), "拒绝非法清单");
}

static void test_policy(void)
{
    const int64_t now = 1760000000LL;
    check(photo_format_action(PHOTO_FMT_BLANK, true, false, true, now, true,
                              now + 600, false) == PHOTO_FMT_GO,
          "BLANK + 有效 token = 格式化");
    check(photo_format_action(PHOTO_FMT_RESUME, true, false, true, now, true,
                              now + 600, false) == PHOTO_FMT_GO,
          "RESUME + 有效 token = 继续格式化");
    check(photo_format_action(PHOTO_FMT_BLANK, false, false, true, now, true,
                              now + 600, false) == PHOTO_FMT_NO,
          "没有 token 不格式化");
    check(photo_format_action(PHOTO_FMT_BLANK, true, true, true, now, true,
                              now + 600, false) == PHOTO_FMT_NO,
          "用过的 token 不重复使用");
    check(photo_format_action(PHOTO_FMT_BLANK, true, false, false, now, true,
                              now + 600, false) == PHOTO_FMT_NO,
          "SNTP 未同步不格式化");
    check(photo_format_action(PHOTO_FMT_BLANK, true, false, true, now, false,
                              0, false) == PHOTO_FMT_NO,
          "缺绝对到期时间不格式化");
    check(photo_format_action(PHOTO_FMT_BLANK, true, false, true, now + 600,
                              true, now + 600, false) == PHOTO_FMT_NO,
          "token 已过期不格式化");
    check(photo_format_action(PHOTO_FMT_CORRUPT, true, false, true, now, true,
                              now + 600, false) == PHOTO_FMT_NO,
          "CORRUPT 无 force 不格式化");
    check(photo_format_action(PHOTO_FMT_CORRUPT, true, false, true, now, true,
                              now + 600, true) == PHOTO_FMT_GO_FORCE,
          "CORRUPT + force = 格式化");
    check(photo_format_action(PHOTO_FMT_READY, true, false, true, now, true,
                              now + 600, true) == PHOTO_FMT_NO,
          "READY 一律拒绝");
    check(photo_format_action(PHOTO_FMT_UNAVAILABLE, true, false, true, now,
                              true, now + 600, true) == PHOTO_FMT_NO,
          "卡级读失败永不格式化");

    check(photo_token_valid("3f2a9c01d4e5b6a7"), "token 合法");
    check(!photo_token_valid("3f2a9c") && !photo_token_valid("zzzzzzzz"),
          "token 长度/字符集校验");

    check(photo_backoff_s(1) == 60 && photo_backoff_s(2) == 300 &&
          photo_backoff_s(3) == -1, "退避 1/5/30 分钟");

    photo_bad_t bad = { 0 };
    char sha[65];
    memset(sha, 'a', 64); sha[64] = 0;
    check(photo_bad_add(&bad, sha), "记录 bad 前缀");
    check(!photo_bad_add(&bad, sha) || bad.count == 1, "bad 去重");
    for (int i = 0; i < 20; i++) {
        memset(sha, 'a', 64);
        sha[0] = "0123456789abcdef"[i % 16];
        sha[1] = "0123456789abcdef"[(i / 16) % 16];
        photo_bad_add(&bad, sha);
    }
    check(bad.count == PHOTO_BAD_MAX, "bad 最多 8 项");
}

static void test_status(void)
{
    photo_bad_t bad = { 0 };
    char sha[65];
    for (int i = 0; i < 8; i++) {
        memset(sha, 'a', 64);
        sha[i] = "01234567"[i];
        sha[64] = 0;
        photo_bad_add(&bad, sha);
    }
    photo_status_t s = {
        .cap = true, .ready = true, .plv = 7, .have = 12,
        .free_kb = 1234, .state = "ok", .init_done = "3f2a9c01", .bad = &bad,
    };
    char buf[512];
    int n = photo_status_json(&s, buf, sizeof buf);
    check(n > 0 && n < 300, "status.photo 在 300B 内");
    check(strstr(buf, "\"cap\":true") && strstr(buf, "\"ready\":true") &&
          strstr(buf, "\"plv\":7") && strstr(buf, "\"have\":12") &&
          strstr(buf, "\"free_kb\":1234") && strstr(buf, "\"state\":\"ok\"") &&
          strstr(buf, "\"init_done\":\"3f2a9c01\""), "status 字段齐全");
    check(strstr(buf, "\"bad\":[\"0aaaaaaa\"") != NULL, "bad 前缀数组");

    char small[64];
    check(photo_status_json(&s, small, sizeof small) == -1, "缓冲不足返回 -1");

    check(strcmp(photo_state_name(PHOTO_ABSENT), "absent") == 0 &&
          strcmp(photo_state_name(PHOTO_UNINIT), "uninit") == 0 &&
          strcmp(photo_state_name(PHOTO_READY), "ok") == 0 &&
          strcmp(photo_state_name(PHOTO_FAULT), "fault") == 0, "state 名称");
}

int main(void)
{
    test_json();
    test_policy();
    test_status();
    if (g_fails) { printf("%d 项失败\n", g_fails); return 1; }
    printf("全部通过\n");
    return 0;
}
