/*
 * 播放清单的流式 JSON 解析（M2）。
 *
 * 清单整份 ≤64KB，但设备稳态空闲堆只有一百多 KB，还要同时扛 MQTT TLS、
 * 取帧 TLS 和推流。所以**不把 body 整块读进堆**：HTTP 每次读 4KB，
 * 解析器按字节吃，只保留要用的字段。
 *
 * 不依赖 ESP-IDF，host 测试与板上跑的是同一份代码。
 *
 * 语义（docs/PHOTO.md 5.2）：
 *   - 顶层：plv、init_token、init_force、init_expires_at（/expires_at）、entries
 *   - entries 是数组；只收 kind=="photo" 的条目，未知 kind 整条忽略
 *   - photo 条目：sha256(64 位小写 hex)、size、duration_s、order
 *   - 数组顺序权威；photo_count 按出现顺序保存
 *
 * 失败一律返回 false，调用方丢弃整份清单——绝不用半份清单去 GC 或下载。
 * 不做动态分配：状态全在 photo_json_t + photo_pl_t 里，由调用方提供。
 */
#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#define PHOTO_PL_MAX_PHOTOS 200
#define PHOTO_PL_MAX_ENTRIES 512
#define PHOTO_PL_HEX 64

typedef struct {
    /* 存二进制而不是 64 位 hex：200 项差 6KB 静态 RAM。hex 只在写日志、
     * URL、bad 前缀时临时转换。 */
    uint8_t  sha256[32];
    uint32_t size;
    uint16_t duration_s;
    uint16_t order;
} photo_pl_entry_t;

typedef struct {
    int      plv;
    bool     has_plv;
    char     init_token[65];
    bool     has_token;
    bool     init_force;
    int64_t  expires_at;
    bool     has_expires;
    bool     entries_seen;
    int      entry_count;
    int      photo_count;
    photo_pl_entry_t photos[PHOTO_PL_MAX_PHOTOS];
} photo_pl_t;

typedef enum {
    PJ_CTX_NONE = 0,
    PJ_CTX_TOP,
    PJ_CTX_ENTRIES,
    PJ_CTX_ENTRY,
    PJ_CTX_SKIP,
} pj_ctx_t;

typedef struct {
    photo_pl_t *out;
    bool        err;
    bool        done;

    int         sp;
    pj_ctx_t    ctx[8];
    bool        is_arr[8];

    /* 字符串是一个「待定 token」：下一个非空白字符是 ':' 就当键，
     * 否则当值。这样跨 chunk 也不需要回退。 */
    bool        pend_str;
    char        str[129];
    int         str_len;
    bool        str_long;

    char        key[32];
    bool        have_key;

    /* 数字/字面量 */
    bool        in_num;
    char        num[24];
    int         num_len;
    bool        in_lit;
    char        lit[8];
    int         lit_len;

    bool        in_str;
    int         esc;          /* 0=无 1=反斜杠后 2=读 4 位 \u */
    uint32_t    ucode;

    /* 正在拼的 entry */
    char        kind[16];
    char        esha[PHOTO_PL_HEX + 1];
    uint32_t    esize;
    bool        esize_set;
    uint32_t    edur;
    uint32_t    eorder;
    bool        efield_bad;
} photo_json_t;

void photo_json_init(photo_json_t *p, photo_pl_t *out);
bool photo_json_feed(photo_json_t *p, const void *data, size_t len);
bool photo_json_finish(photo_json_t *p);

/* 64 位小写 hex → 32 字节；成功返回 true。 */
bool photo_sha_from_hex(const char hex[PHOTO_PL_HEX], uint8_t out[32]);
