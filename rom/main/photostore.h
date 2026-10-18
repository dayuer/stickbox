#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

/* 照片裸扇区存储层（M1）。
 *
 * 布局（整张专用卡，扇区 512B）：
 *   扇区 0/1     超级块 A/B："SBPH" + version + seq + 布局 + CRC32。
 *                取 seq 最大且 CRC 合法的一份；没有合法超级块 = 未初始化。
 *   索引区       256 × 64B：sha256(32) | size(4) | start_sector(4) |
 *                status(1) | duration_s(2) | order(2) | reserved(4) |
 *                pad(11) | CRC32(4)。CRC 覆盖前 60 字节。
 *   数据区       从 data_start 开始，按 4KB（8 扇区）对齐，first-fit 分配。
 *
 * 铁律：
 *   - 没有合法超级块时只读不写；format 必须由上层在用户确认后显式调用。
 *   - 写入顺序：数据 → sha256 读回校验 → 索引项（提交点，最后写）。
 *   - 所有写入都限制在超级块/索引区/数据区内（本方案命名空间）。
 *
 * 这个文件不依赖 ESP-IDF：IO 与底层通过回调注入，host 测试共用同一份逻辑。
 */

#define PHOTO_INDEX_COUNT   256
#define PHOTO_ENTRY_SIZE      64
#define PHOTO_INDEX_START      2
#define PHOTO_INDEX_BYTES   (PHOTO_INDEX_COUNT * PHOTO_ENTRY_SIZE)
#define PHOTO_INDEX_SECTORS (PHOTO_INDEX_BYTES / 512)
#define PHOTO_ALIGN_SECTORS    8          /* 4KB */
#define PHOTO_SB_MAGIC       "SBPH"
#define PHOTO_SB_VERSION       1
#define PHOTO_MAX_PHOTO_SIZE (16u * 1024 * 1024)

typedef enum {
    PHOTO_ABSENT = 0,   /* 没有卡/初始化失败 */
    PHOTO_UNINIT,       /* 卡在，但两个超级块都没有魔数；等 init_token */
    PHOTO_READY,        /* 可用 */
    PHOTO_FAULT,        /* IO 错误，或魔数存在但 CRC 全坏 */
} photo_state_t;

/* format 前必须区分的卡状态：
 *   BLANK   两个超级块都没有魔数 → init_token 可格式化
 *   RESUME  上次格式化中断（超级块有效但带 FORMATTING）→ token 可续做
 *   CORRUPT 魔数存在但没有一份 CRC 合法 → 只有 token.force 才允许
 *   READY   已有合法布局 → 一律不允许 format
 *   UNAVAILABLE 没卡/读失败 → 永不格式化
 */
typedef enum {
    PHOTO_FMT_UNAVAILABLE = 0,
    PHOTO_FMT_BLANK,
    PHOTO_FMT_RESUME,
    PHOTO_FMT_CORRUPT,
    PHOTO_FMT_READY,
} photo_format_mode_t;

typedef enum {
    PHOTO_ENTRY_FREE = 0,
    PHOTO_ENTRY_VALID = 1,
    PHOTO_ENTRY_BAD = 2,     /* 预留：M2 下载失败标记 */
} photo_entry_status_t;

typedef struct {
    void *ctx;
    bool (*read)(void *ctx, uint32_t sector, void *buf, uint32_t nsec);
    bool (*write)(void *ctx, uint32_t sector, const void *buf, uint32_t nsec);
    uint32_t sector_count;
} photo_io_t;

typedef struct {
    uint8_t  sha256[32];
    uint32_t size;
    uint32_t start_sector;
    uint8_t  status;
    uint16_t duration_s;
    uint16_t order;
} photo_entry_t;

typedef struct {
    uint32_t data_start;   /* sector */
    uint32_t data_end;     /* exclusive */
    uint32_t card_sectors;
    uint32_t sb_seq;
    int      sb_slot;      /* 0=A, 1=B, -1=无 */
    bool     magic_seen;   /* 任一超级块有 "SBPH" */
} photo_layout_t;

typedef struct photostore {
    /* **按值保存** IO 表。open() 只拷贝，不保留调用方指针：调用方在栈上
     * 放一个局部 photo_io_t 是天然写法，指针存下来会在函数返回后悬空
     * （M2 上板踩过：格式化时 PC=0）。 */
    photo_io_t io;
    uint8_t *buf;          /* 调用方提供的 ≥4KB 缓冲 */
    uint32_t buf_len;
    photo_state_t state;
    photo_layout_t lay;
    int      cache_sector;  /* 索引缓存：-1 无 */
    uint32_t reads, writes; /* 扇区操作计数（host 测扫描次数） */
} photostore_t;

typedef struct {
    uint32_t start_sector;
    uint32_t size;
    uint32_t offset;
    bool     valid;
} photo_file_t;

typedef struct ps_put ps_put_t;

/* 打开：读超级块 A/B。无 IO/读失败 → ABSENT；无合法超级块 → UNINIT。 */
bool photostore_open(photostore_t *st, const photo_io_t *io, uint8_t *buf4k,
                     uint32_t buf_len);
void photostore_close(photostore_t *st);
photo_state_t photostore_state(const photostore_t *st);

photo_format_mode_t photostore_format_mode(const photostore_t *st);

/* 格式化：把索引区清零，再交替写超级块（A/B，seq+1）。
 * force=true 只用于“魔数存在但 CRC 全坏”的卡（token.force）。
 * READY 状态拒绝 format；UNAVAILABLE 永不 format。
 * **调用方必须先拿到有效 init_token**；本层不会自动调用。 */
bool photostore_format(photostore_t *st, bool force);

/* 写入：first-fit 分配 4KB 对齐块；sha256 去重。
 * 返回 false 且 exists=true 表示内容已存在（调用方可用 update_meta 改属性）。 */
bool photostore_put_begin(photostore_t *st, const uint8_t sha256[32],
                          uint32_t size, uint16_t duration_s, uint16_t order,
                          ps_put_t **put, bool *exists);
bool photostore_put_write(ps_put_t *p, const void *data, uint32_t len);
bool photostore_put_finish(ps_put_t *p);
void photostore_put_abort(ps_put_t *p);

bool photostore_get(photostore_t *st, const uint8_t sha256[32], photo_file_t *f);
/* 顺序读：最多 cap 字节，f->offset 前进；返回实际字节数，-1 出错。 */
int photostore_read(photostore_t *st, photo_file_t *f, void *dst, uint32_t cap);

bool photostore_remove(photostore_t *st, const uint8_t sha256[32]);
bool photostore_update_meta(photostore_t *st, const uint8_t sha256[32],
                            uint16_t duration_s, uint16_t order);

typedef bool (*photo_walk_cb)(const photo_entry_t *e, void *arg);
/* 遍历有效项（按索引顺序）。返回遍历到的项数；*bad 返回 CRC 损坏项数。 */
int photostore_walk(photostore_t *st, photo_walk_cb cb, void *arg,
                    uint32_t *bad);
/* 空闲/总数据字节（4KB 块粒度）、有效项数。 */
bool photostore_space(photostore_t *st, uint32_t *free_bytes,
                      uint32_t *total_bytes, uint16_t *used_entries);
/* 只校验索引项 CRC（不读数据），返回有效项数，*bad=损坏项数。 */
int photostore_scan_index(photostore_t *st, uint32_t *bad);
