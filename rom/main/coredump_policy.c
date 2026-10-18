#include "coredump_policy.h"

cd_class_t cd_classify(int http_status)
{
    if (http_status >= 200 && http_status < 300) return CD_OK;
    if (http_status == 408 || http_status == 429) return CD_TRANSIENT;
    if (http_status >= 400 && http_status < 500) return CD_PERMANENT;
    return CD_TRANSIENT;
}

bool cd_should_try(cd_state_t *st, uint32_t fp, uint32_t today)
{
    if (st->fp != fp) {
        st->fp = fp;
        st->day = today;
        st->tries = 0;
    } else if (today != CD_DAY_UNKNOWN && today != st->day) {
        /* 新的一天（或从「未知日」第一次拿到真实日期）：重新给 3 次。 */
        st->day = today;
        st->tries = 0;
    }
    return st->tries < CD_MAX_TRIES_PER_DAY;
}

void cd_note_attempt(cd_state_t *st, int http_status)
{
    if (cd_classify(http_status) == CD_OK) {
        st->tries = 0;   /* 调用方会擦镜像；计数清零以免影响下一份镜像 */
        return;
    }
    if (st->tries < 255) st->tries++;
}

uint32_t cd_day_of(int64_t unix_s)
{
    /* 2025-01-01T00:00:00Z = 1735689600 */
    if (unix_s < 1735689600LL) return CD_DAY_UNKNOWN;
    return (uint32_t)(unix_s / 86400);
}
