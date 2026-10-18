/* 崩溃镜像上传退避的主机测试。板上跑的是同一份 coredump_policy.c。 */
#include <stdio.h>
#include <stdlib.h>

#include "coredump_policy.h"

static int fails;
#define CHECK(c, msg) do { if (!(c)) { printf("✗ %s\n", msg); fails++; } } while (0)

static const uint32_t DAY = 20380;   /* 2025 之后的某天 */

int main(void)
{
    /* 分类 */
    CHECK(cd_classify(200) == CD_OK, "200 是成功");
    CHECK(cd_classify(204) == CD_OK, "204 是成功");
    CHECK(cd_classify(413) == CD_PERMANENT, "413 是永久失败（这次事故的那个码）");
    CHECK(cd_classify(400) == CD_PERMANENT, "400 是永久失败");
    CHECK(cd_classify(403) == CD_PERMANENT, "403 是永久失败");
    CHECK(cd_classify(429) == CD_TRANSIENT, "429 可重试");
    CHECK(cd_classify(408) == CD_TRANSIENT, "408 可重试");
    CHECK(cd_classify(503) == CD_TRANSIENT, "5xx 可重试");
    CHECK(cd_classify(-1) == CD_TRANSIENT, "网络错误可重试");

    /* 回归：413 每次开机都重试是这次的事故。同一镜像同一天最多 3 次。 */
    cd_state_t st = {0};
    int tried = 0;
    for (int boot = 0; boot < 10; boot++) {
        if (cd_should_try(&st, 0xABCD, DAY)) { cd_note_attempt(&st, 413); tried++; }
    }
    CHECK(tried == CD_MAX_TRIES_PER_DAY, "413：同一天 10 次开机只真正上传 3 次");

    /* 第二天重新给 3 次 */
    tried = 0;
    for (int boot = 0; boot < 10; boot++)
        if (cd_should_try(&st, 0xABCD, DAY + 1)) { cd_note_attempt(&st, 413); tried++; }
    CHECK(tried == 3, "次日重新给 3 次");

    /* 换了新镜像：立刻重新计数，不受旧镜像的配额影响 */
    CHECK(cd_should_try(&st, 0x1234, DAY + 1), "新镜像同一天也能马上试");

    /* 5xx 也受同一上限（不能让服务端宕机变成每次开机一次 TLS） */
    cd_state_t s5 = {0};
    tried = 0;
    for (int boot = 0; boot < 10; boot++)
        if (cd_should_try(&s5, 1, DAY)) { cd_note_attempt(&s5, 503); tried++; }
    CHECK(tried == 3, "5xx 同一天也最多 3 次");

    /* 成功后计数清零 */
    cd_state_t ok = {0};
    cd_should_try(&ok, 7, DAY); cd_note_attempt(&ok, 413);
    cd_should_try(&ok, 7, DAY); cd_note_attempt(&ok, 201);
    CHECK(ok.tries == 0, "成功后计数清零");

    /* 时间未同步：不按日期重置，仍受 3 次上限 */
    cd_state_t su = {0};
    tried = 0;
    for (int boot = 0; boot < 10; boot++)
        if (cd_should_try(&su, 9, CD_DAY_UNKNOWN)) { cd_note_attempt(&su, -1); tried++; }
    CHECK(tried == 3, "时间未知时 10 次开机也只试 3 次");
    CHECK(cd_should_try(&su, 9, DAY), "拿到真实日期后重新给 3 次");

    /* 没发出去（堆不够）的那次不调 note：不消耗配额 */
    cd_state_t sh = {0};
    for (int i = 0; i < 5; i++) CHECK(cd_should_try(&sh, 3, DAY), "只问不记，不消耗配额");

    /* 日期换算 */
    CHECK(cd_day_of(0) == CD_DAY_UNKNOWN, "1970 视为未同步");
    CHECK(cd_day_of(1700000000) == CD_DAY_UNKNOWN, "2023 视为未同步");
    CHECK(cd_day_of(1792320000) == 1792320000 / 86400, "2026 正常换算");

    if (fails) { printf("coredump_policy_test: %d 项失败\n", fails); return 1; }
    printf("coredump_policy_test: 全部通过\n");
    return 0;
}
