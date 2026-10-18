/* 照片裸扇区存储核心（M1）。纯 C，IO 回调注入，host 与固件共用。 */
#include "photostore.h"

#include <stdlib.h>
#include <string.h>

#include "photosha.h"

#define SB_A_SECTOR 0
#define SB_B_SECTOR 1
#define SB_FLAG_FORMATTING 0x0001u

/* ── CRC32（IEEE，与 zlib 一致） ── */
static uint32_t crc32_update(uint32_t crc, const uint8_t *p, size_t n)
{
    crc = ~crc;
    for (size_t i = 0; i < n; i++) {
        crc ^= p[i];
        for (int k = 0; k < 8; k++) crc = (crc >> 1) ^ (0xEDB88320u & -(crc & 1));
    }
    return ~crc;
}

static void put32(uint8_t *p, uint32_t v)
{
    p[0] = v; p[1] = v >> 8; p[2] = v >> 16; p[3] = v >> 24;
}
static uint32_t get32(const uint8_t *p)
{
    return (uint32_t)p[0] | ((uint32_t)p[1] << 8) |
           ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24);
}
static void put16(uint8_t *p, uint16_t v) { p[0] = v; p[1] = v >> 8; }
static uint16_t get16(const uint8_t *p) { return (uint16_t)(p[0] | (p[1] << 8)); }

/* ── 超级块 ── */
typedef struct {
    uint32_t seq;
    uint16_t flags;
    uint32_t data_start;
    uint32_t data_end;
    uint32_t card_sectors;
    bool valid;
} sb_t;

static void sb_encode(uint8_t b[512], const sb_t *sb)
{
    memset(b, 0, 512);
    memcpy(b, PHOTO_SB_MAGIC, 4);
    put16(b + 4, PHOTO_SB_VERSION);
    put16(b + 6, sb->flags);
    put32(b + 8, sb->seq);
    put32(b + 12, PHOTO_INDEX_START);
    put16(b + 16, PHOTO_INDEX_COUNT);
    put16(b + 18, PHOTO_ENTRY_SIZE);
    put32(b + 20, sb->data_start);
    put32(b + 24, sb->data_end);
    put32(b + 28, sb->card_sectors);
    put32(b + 508, crc32_update(0, b, 508));
}

static sb_t sb_decode(const uint8_t b[512], uint32_t card_sectors)
{
    sb_t sb = { 0 };
    if (memcmp(b, PHOTO_SB_MAGIC, 4) != 0) return sb;
    if (get16(b + 4) != PHOTO_SB_VERSION) return sb;
    if (get32(b + 508) != crc32_update(0, b, 508)) return sb;
    sb.seq = get32(b + 8);
    sb.flags = get16(b + 6);
    uint32_t idx_start = get32(b + 12);
    uint16_t count = get16(b + 16);
    uint16_t esz = get16(b + 18);
    sb.data_start = get32(b + 20);
    sb.data_end = get32(b + 24);
    sb.card_sectors = get32(b + 28);
    if (idx_start != PHOTO_INDEX_START || count != PHOTO_INDEX_COUNT ||
        esz != PHOTO_ENTRY_SIZE) return sb;
    if (sb.data_start % PHOTO_ALIGN_SECTORS != 0) return sb;
    if (sb.data_start < PHOTO_INDEX_START + PHOTO_INDEX_SECTORS) return sb;
    if (sb.data_end <= sb.data_start + PHOTO_ALIGN_SECTORS) return sb;
    if (sb.data_end > card_sectors || sb.card_sectors != card_sectors) return sb;
    sb.valid = true;
    return sb;
}

static bool io_read(photostore_t *st, uint32_t sector, void *buf, uint32_t nsec)
{
    st->reads++;
    if (!st->io->read(st->io->ctx, sector, buf, nsec)) {
        st->state = PHOTO_FAULT;
        return false;
    }
    return true;
}

static bool io_write(photostore_t *st, uint32_t sector, const void *buf, uint32_t nsec)
{
    st->writes++;
    if (!st->io->write(st->io->ctx, sector, buf, nsec)) {
        st->state = PHOTO_FAULT;
        return false;
    }
    return true;
}

static bool sb_write(photostore_t *st, int slot, const sb_t *sb)
{
    uint8_t b[512];
    sb_encode(b, sb);
    uint32_t sec = slot == 0 ? SB_A_SECTOR : SB_B_SECTOR;
    if (!io_write(st, sec, b, 1)) return false;
    uint8_t rb[512];
    if (!io_read(st, sec, rb, 1)) return false;
    return memcmp(b, rb, 512) == 0;
}

static bool valid_layout(uint32_t sectors, uint32_t *data_start)
{
    if (sectors < PHOTO_INDEX_START + PHOTO_INDEX_SECTORS + PHOTO_ALIGN_SECTORS)
        return false;
    uint32_t ds = PHOTO_INDEX_START + PHOTO_INDEX_SECTORS;
    ds = (ds + PHOTO_ALIGN_SECTORS - 1) / PHOTO_ALIGN_SECTORS * PHOTO_ALIGN_SECTORS;
    *data_start = ds;
    return true;
}

bool photostore_open(photostore_t *st, const photo_io_t *io, uint8_t *buf4k,
                     uint32_t buf_len)
{
    memset(st, 0, sizeof *st);
    st->cache_sector = -1;
    st->io = io;
    st->buf = buf4k;
    st->buf_len = buf_len;
    if (!io || !io->read || !io->write || !buf4k || buf_len < 4096) {
        st->state = PHOTO_ABSENT;
        return false;
    }
    uint32_t ds = 0;
    if (!valid_layout(io->sector_count, &ds)) {
        st->state = PHOTO_ABSENT;
        return false;
    }

    uint8_t a[512], b[512];
    if (!st->io->read(io->ctx, SB_A_SECTOR, a, 1) ||
        !st->io->read(io->ctx, SB_B_SECTOR, b, 1)) {
        st->state = PHOTO_ABSENT;   /* 读不了卡，不是“未初始化” */
        return false;
    }
    st->reads += 2;
    bool magic_a = memcmp(a, PHOTO_SB_MAGIC, 4) == 0;
    bool magic_b = memcmp(b, PHOTO_SB_MAGIC, 4) == 0;
    st->lay.magic_seen = magic_a || magic_b;
    sb_t sa = sb_decode(a, io->sector_count);
    sb_t sb = sb_decode(b, io->sector_count);
    sb_t pick = { 0 };
    int slot = -1;
    if (sa.valid) { pick = sa; slot = 0; }
    if (sb.valid && (!pick.valid || sb.seq > pick.seq)) { pick = sb; slot = 1; }
    if (!pick.valid) {
        /* 有魔数但 CRC 全坏 = fault（只有 token.force 才能格式化）；
         * 完全没有魔数才是 blank/uninit。 */
        st->state = st->lay.magic_seen ? PHOTO_FAULT : PHOTO_UNINIT;
        st->lay.data_start = ds;
        st->lay.data_end = io->sector_count;
        st->lay.card_sectors = io->sector_count;
        st->lay.sb_seq = 0;
        st->lay.sb_slot = -1;
        return false;
    }
    st->lay.data_start = pick.data_start;
    st->lay.data_end = pick.data_end;
    st->lay.card_sectors = pick.card_sectors;
    st->lay.sb_seq = pick.seq;
    st->lay.sb_slot = slot;
    st->state = (pick.flags & SB_FLAG_FORMATTING) ? PHOTO_UNINIT : PHOTO_READY;
    return st->state == PHOTO_READY;
}

void photostore_close(photostore_t *st) { memset(st, 0, sizeof *st); }

photo_state_t photostore_state(const photostore_t *st) { return st->state; }

photo_format_mode_t photostore_format_mode(const photostore_t *st)
{
    if (!st->io || !st->buf) return PHOTO_FMT_UNAVAILABLE;
    if (st->state == PHOTO_READY) return PHOTO_FMT_READY;
    if (st->state == PHOTO_ABSENT) return PHOTO_FMT_UNAVAILABLE;
    if (st->state == PHOTO_UNINIT)
        return st->lay.magic_seen ? PHOTO_FMT_RESUME : PHOTO_FMT_BLANK;
    if (st->state == PHOTO_FAULT && st->lay.magic_seen) return PHOTO_FMT_CORRUPT;
    return PHOTO_FMT_UNAVAILABLE;
}

bool photostore_format(photostore_t *st, bool force)
{
    photo_format_mode_t mode = photostore_format_mode(st);
    if (mode == PHOTO_FMT_READY || mode == PHOTO_FMT_UNAVAILABLE) return false;
    if (mode == PHOTO_FMT_CORRUPT && !force) return false;

    /* 1) 先写“正在格式化”的超级块：掉电会停在 UNINIT，不会把半清空的
     *    索引当成有效数据。2) 清索引区。3) 写最终超级块（提交点）。 */
    int slot = st->lay.sb_slot == 0 ? 1 : 0;
    uint32_t seq = st->lay.sb_seq;
    sb_t sb = {
        .seq = seq + 1, .flags = SB_FLAG_FORMATTING,
        .data_start = st->lay.data_start,
        .data_end = st->lay.card_sectors,
        .card_sectors = st->lay.card_sectors,
    };
    if (!sb_write(st, slot, &sb)) return false;

    st->cache_sector = -1;
    memset(st->buf, 0, 4096);
    for (uint32_t s = 0; s < PHOTO_INDEX_SECTORS; s += 8) {
        if (!io_write(st, PHOTO_INDEX_START + s, st->buf, 8)) return false;
    }
    st->cache_sector = -1;

    sb.seq = seq + 2;
    sb.flags = 0;
    if (!sb_write(st, slot, &sb)) return false;

    st->lay.sb_seq = sb.seq;
    st->lay.sb_slot = slot;
    st->lay.magic_seen = true;
    st->state = PHOTO_READY;
    return true;
}

/* ── 索引项 ── */
static bool entry_zero(const uint8_t e[64])
{
    for (int i = 0; i < 60; i++) if (e[i]) return false;
    return true;
}

static void entry_encode(uint8_t e[64], const photo_entry_t *x)
{
    memset(e, 0, 64);
    memcpy(e, x->sha256, 32);
    put32(e + 32, x->size);
    put32(e + 36, x->start_sector);
    e[40] = x->status;
    put16(e + 41, x->duration_s);
    put16(e + 43, x->order);
    put32(e + 60, crc32_update(0, e, 60));
}

static bool entry_decode(const uint8_t e[64], photo_entry_t *x)
{
    if (entry_zero(e)) {
        memset(x, 0, sizeof *x);
        x->status = PHOTO_ENTRY_FREE;
        return true;
    }
    if (get32(e + 60) != crc32_update(0, e, 60)) return false;
    memcpy(x->sha256, e, 32);
    x->size = get32(e + 32);
    x->start_sector = get32(e + 36);
    x->status = e[40];
    x->duration_s = get16(e + 41);
    x->order = get16(e + 43);
    return true;
}

static bool idx_read(photostore_t *st, int i, photo_entry_t *e)
{
    if (i < 0 || i >= PHOTO_INDEX_COUNT) return false;
    uint32_t byte = (uint32_t)i * PHOTO_ENTRY_SIZE;
    uint32_t sec = PHOTO_INDEX_START + byte / 512;
    if (st->cache_sector != (int)sec) {
        if (!io_read(st, sec, st->buf, 1)) return false;
        st->cache_sector = (int)sec;
    }
    return entry_decode(st->buf + byte % 512, e);
}

static bool idx_write(photostore_t *st, int i, const photo_entry_t *e)
{
    if (i < 0 || i >= PHOTO_INDEX_COUNT) return false;
    uint32_t byte = (uint32_t)i * PHOTO_ENTRY_SIZE;
    uint32_t sec = PHOTO_INDEX_START + byte / 512;
    if (st->cache_sector != (int)sec) {
        if (!io_read(st, sec, st->buf, 1)) return false;
        st->cache_sector = (int)sec;
    }
    uint8_t enc[64];
    entry_encode(enc, e);
    memcpy(st->buf + byte % 512, enc, 64);
    if (!io_write(st, sec, st->buf, 1)) return false;
    return true;
}

static int idx_find(photostore_t *st, const uint8_t sha[32], photo_entry_t *out)
{
    for (int i = 0; i < PHOTO_INDEX_COUNT; i++) {
        photo_entry_t e;
        if (!idx_read(st, i, &e)) continue;
        if (e.status == PHOTO_ENTRY_VALID && memcmp(e.sha256, sha, 32) == 0) {
            if (out) *out = e;
            return i;
        }
    }
    return -1;
}

/* 4KB 块分配：收集已占用块，first-fit。 */
typedef struct { uint32_t start, blocks; } span_t;

static int collect_spans(photostore_t *st, span_t *sp, int max)
{
    int n = 0;
    uint32_t ds = st->lay.data_start, de = st->lay.data_end;
    for (int i = 0; i < PHOTO_INDEX_COUNT && n < max; i++) {
        photo_entry_t e;
        if (!idx_read(st, i, &e)) continue;
        if (e.status == PHOTO_ENTRY_FREE) continue;
        if (e.start_sector < ds || e.start_sector >= de) continue;
        uint32_t blocks = (e.size + 4095) / 4096;
        sp[n].start = e.start_sector;
        sp[n].blocks = blocks;
        n++;
    }
    /* 插入排序（≤256 项） */
    for (int i = 1; i < n; i++) {
        span_t k = sp[i];
        int j = i - 1;
        while (j >= 0 && sp[j].start > k.start) { sp[j + 1] = sp[j]; j--; }
        sp[j + 1] = k;
    }
    return n;
}

static bool alloc_blocks(photostore_t *st, uint32_t blocks, uint32_t *out)
{
    span_t sp[PHOTO_INDEX_COUNT];
    int n = collect_spans(st, sp, PHOTO_INDEX_COUNT);
    uint32_t cur = st->lay.data_start;
    for (int i = 0; i < n; i++) {
        if (sp[i].start >= cur && (sp[i].start - cur) / PHOTO_ALIGN_SECTORS >= blocks) {
            *out = cur;
            return true;
        }
        uint32_t end = sp[i].start + sp[i].blocks * PHOTO_ALIGN_SECTORS;
        if (end > cur) cur = end;
    }
    if (st->lay.data_end >= cur &&
        (st->lay.data_end - cur) / PHOTO_ALIGN_SECTORS >= blocks) {
        *out = cur;
        return true;
    }
    return false;
}

/* ── 流式写入 ── */
struct ps_put {
    photostore_t *st;
    uint8_t sha[32];
    uint32_t size;
    uint16_t duration_s, order;
    uint32_t start, blocks;
    uint32_t written, flushed, fill;
    photosha_ctx hash;
    bool failed;
};

bool photostore_put_begin(photostore_t *st, const uint8_t sha256[32],
                          uint32_t size, uint16_t duration_s, uint16_t order,
                          ps_put_t **put, bool *exists)
{
    if (put) *put = NULL;
    if (exists) *exists = false;
    if (st->state != PHOTO_READY || size == 0 || size > PHOTO_MAX_PHOTO_SIZE)
        return false;
    if (idx_find(st, sha256, NULL) >= 0) {
        if (exists) *exists = true;
        return true;
    }
    uint32_t blocks = (size + 4095) / 4096;
    uint32_t start = 0;
    if (!alloc_blocks(st, blocks, &start)) return false;

    ps_put_t *p = calloc(1, sizeof *p);
    if (!p) return false;
    p->st = st;
    memcpy(p->sha, sha256, 32);
    p->size = size;
    p->duration_s = duration_s;
    p->order = order;
    p->start = start;
    p->blocks = blocks;
    photosha_init(&p->hash);
    *put = p;
    return true;
}

bool photostore_put_write(ps_put_t *p, const void *data, uint32_t len)
{
    if (!p || p->failed) return false;
    if (len > p->size - p->written) return false;
    photosha_update(&p->hash, data, len);

    const uint8_t *d = data;
    uint32_t left = len;
    while (left) {
        uint32_t k = 4096 - p->fill;
        if (k > left) k = left;
        memcpy(p->st->buf + p->fill, d, k);
        p->fill += k; d += k; left -= k;
        if (p->fill == 4096) {
            if (!io_write(p->st, p->start + p->flushed / 512, p->st->buf, 8)) {
                p->failed = true;
                return false;
            }
            p->st->cache_sector = -1;   /* buf 已用作数据缓冲 */
            p->flushed += 4096;
            p->fill = 0;
        }
    }
    p->written += len;
    return true;
}

bool photostore_put_finish(ps_put_t *p)
{
    if (!p || p->failed) { photostore_put_abort(p); return false; }
    if (p->written != p->size) { photostore_put_abort(p); return false; }

    if (p->fill) {
        uint32_t secs = (p->fill + 511) / 512;
        memset(p->st->buf + p->fill, 0, secs * 512 - p->fill);
        p->st->cache_sector = -1;
        if (!io_write(p->st, p->start + p->flushed / 512, p->st->buf, secs)) {
            photostore_put_abort(p);
            return false;
        }
    }

    /* 写后读回：重新计算 sha256，必须等于请求的 sha。 */
    photosha_ctx h;
    photosha_init(&h);
    p->st->cache_sector = -1;
    uint32_t off = 0;
    while (off < p->size) {
        uint32_t n = p->size - off < 4096 ? p->size - off : 4096;
        uint32_t secs = (n + 511) / 512;
        if (!io_read(p->st, p->start + off / 512, p->st->buf, secs)) {
            photostore_put_abort(p);
            return false;
        }
        photosha_update(&h, p->st->buf, n);
        off += n;
    }
    uint8_t got[32];
    photosha_final(&h, got);
    if (memcmp(got, p->sha, 32) != 0) {
        photostore_put_abort(p);
        return false;
    }

    /* 找空索引项并提交（提交点，最后写）。 */
    int slot = -1;
    for (int i = 0; i < PHOTO_INDEX_COUNT; i++) {
        photo_entry_t e;
        if (!idx_read(p->st, i, &e)) { photostore_put_abort(p); return false; }
        if (e.status == PHOTO_ENTRY_FREE) { slot = i; break; }
    }
    if (slot < 0) { photostore_put_abort(p); return false; }

    photo_entry_t e = { 0 };
    memcpy(e.sha256, p->sha, 32);
    e.size = p->size;
    e.start_sector = p->start;
    e.status = PHOTO_ENTRY_VALID;
    e.duration_s = p->duration_s;
    e.order = p->order;
    if (!idx_write(p->st, slot, &e)) { photostore_put_abort(p); return false; }

    photo_entry_t rb;
    if (!idx_read(p->st, slot, &rb) || rb.status != PHOTO_ENTRY_VALID ||
        memcmp(rb.sha256, p->sha, 32) != 0 || rb.size != p->size ||
        rb.start_sector != p->start) {
        photostore_put_abort(p);
        return false;
    }
    free(p);
    return true;
}

void photostore_put_abort(ps_put_t *p) { free(p); }

bool photostore_get(photostore_t *st, const uint8_t sha256[32], photo_file_t *f)
{
    if (st->state != PHOTO_READY) return false;
    photo_entry_t e;
    if (idx_find(st, sha256, &e) < 0) return false;
    f->start_sector = e.start_sector;
    f->size = e.size;
    f->offset = 0;
    f->valid = true;
    return true;
}

int photostore_read(photostore_t *st, photo_file_t *f, void *dst, uint32_t cap)
{
    if (!f->valid || st->state != PHOTO_READY || f->offset >= f->size) return 0;
    uint32_t n = f->size - f->offset;
    if (n > cap) n = cap;
    uint8_t *out = dst;
    uint32_t done = 0;
    while (done < n) {
        uint32_t pos = f->offset + done;
        uint32_t sec = f->start_sector + pos / 512;
        uint32_t in = pos % 512;
        uint32_t want = n - done;
        uint32_t room = 4096 - in;
        if (want > room) want = room;
        uint32_t secs = (in + want + 511) / 512;
        st->cache_sector = -1;
        if (!io_read(st, sec, st->buf, secs)) return -1;
        memcpy(out + done, st->buf + in, want);
        done += want;
    }
    f->offset += n;
    return (int)n;
}

bool photostore_remove(photostore_t *st, const uint8_t sha256[32])
{
    if (st->state != PHOTO_READY) return false;
    int i = idx_find(st, sha256, NULL);
    if (i < 0) return false;
    photo_entry_t e = { 0 };
    e.status = PHOTO_ENTRY_FREE;
    return idx_write(st, i, &e);
}

bool photostore_update_meta(photostore_t *st, const uint8_t sha256[32],
                            uint16_t duration_s, uint16_t order)
{
    if (st->state != PHOTO_READY) return false;
    photo_entry_t e;
    int i = idx_find(st, sha256, &e);
    if (i < 0) return false;
    if (e.duration_s == duration_s && e.order == order) return true;
    e.duration_s = duration_s;
    e.order = order;
    return idx_write(st, i, &e);
}

int photostore_walk(photostore_t *st, photo_walk_cb cb, void *arg, uint32_t *bad)
{
    int n = 0;
    uint32_t b = 0;
    for (int i = 0; i < PHOTO_INDEX_COUNT; i++) {
        photo_entry_t e;
        if (!idx_read(st, i, &e)) { b++; continue; }
        if (e.status == PHOTO_ENTRY_VALID) {
            n++;
            if (cb && !cb(&e, arg)) break;
        } else if (e.status != PHOTO_ENTRY_FREE) {
            b++;
        }
    }
    if (bad) *bad = b;
    return n;
}

bool photostore_space(photostore_t *st, uint32_t *free_bytes,
                      uint32_t *total_bytes, uint16_t *used_entries)
{
    if (st->state != PHOTO_READY) return false;
    uint32_t total_blocks = (st->lay.data_end - st->lay.data_start) / PHOTO_ALIGN_SECTORS;
    uint32_t used_blocks = 0;
    uint16_t used = 0;
    for (int i = 0; i < PHOTO_INDEX_COUNT; i++) {
        photo_entry_t e;
        if (!idx_read(st, i, &e)) continue;
        if (e.status == PHOTO_ENTRY_FREE) continue;
        used_blocks += (e.size + 4095) / 4096;
        if (e.status == PHOTO_ENTRY_VALID) used++;
    }
    if (used_blocks > total_blocks) used_blocks = total_blocks;
    if (total_bytes) *total_bytes = total_blocks * 4096;
    if (free_bytes) *free_bytes = (total_blocks - used_blocks) * 4096;
    if (used_entries) *used_entries = used;
    return true;
}

int photostore_scan_index(photostore_t *st, uint32_t *bad)
{
    int valid = 0;
    uint32_t b = 0;
    for (int i = 0; i < PHOTO_INDEX_COUNT; i++) {
        photo_entry_t e;
        if (!idx_read(st, i, &e)) { b++; continue; }
        if (e.status == PHOTO_ENTRY_VALID) valid++;
        else if (e.status != PHOTO_ENTRY_FREE) b++;
    }
    if (bad) *bad = b;
    return valid;
}
