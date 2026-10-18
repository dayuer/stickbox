/*
 * 照片链路的纯策略（M2）：初始化授权、退避、失败列表、status.photo JSON。
 *
 * 不依赖 ESP-IDF，host 测试直接跑这一份。所有输入都当不可信：
 * 只有明确满足条件的 token 才会得到 FORMAT 结论。
 */
#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "photostore.h"

#define PHOTO_BAD_MAX 8

typedef enum {
    PHOTO_FMT_NO = 0,
    PHOTO_FMT_GO,        /* 无 force */
    PHOTO_FMT_GO_FORCE,  /* 带 force（只用于 CRC 全坏的卡） */
} photo_fmt_action_t;

/* 授权规则（docs/PHOTO.md 3、5.2）：
 *   - BLANK / RESUME：token 未过期、未用过即可格式化
 *   - CORRUPT：还要求 token 带 force:true
 *   - READY / UNAVAILABLE：一律不格式化
 *   - has_expires=false 或 time_valid=false（SNTP 未同步）：一律不格式化
 */
photo_fmt_action_t photo_format_action(photo_format_mode_t mode, bool has_token,
                                       bool token_used, bool time_valid,
                                       int64_t now, bool has_expires,
                                       int64_t expires, bool force_flag);

/* token 只接受 8–64 位十六进制（大小写不敏感）——它会被写进日志和
 * status.photo.init_done，不能是任意字符串。 */
bool photo_token_valid(const char *tok);

/* 连续失败次数 → 退避秒数；>=3 次返回 -1（标记跳过，不再重试）。 */
int photo_backoff_s(int fails);

typedef struct {
    int  count;
    char prefix[PHOTO_BAD_MAX][9];
} photo_bad_t;

/* 记一个 sha256 前 8 位（小写六边形）。去重；满了返回 false（仍保留前 8 个）。 */
bool photo_bad_add(photo_bad_t *b, const char sha_hex[65]);
bool photo_bad_has(const photo_bad_t *b, const char prefix[9]);

const char *photo_state_name(photo_state_t st);

typedef struct {
    bool cap;
    bool ready;
    int  plv;
    int  have;
    uint32_t free_kb;
    const char *state;       /* ok / uninit / fault / absent */
    const char *init_done;   /* token 前 8 位，没有则空串 */
    const photo_bad_t *bad;
} photo_status_t;

/* 生成 "photo" 对象的内容（不含外层 "photo":）。成功返回长度，失败 -1。
 * 整条心跳 ≤512B 由调用方保证；本对象控制在 300B 内。 */
int photo_status_json(const photo_status_t *s, char *out, size_t cap);
