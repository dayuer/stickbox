/*
 * photo 裸扇区存储层 host 测试（M1）。
 *
 * 内存模拟整卡扇区，并注入写失败模拟掉电；覆盖：
 *   - 未初始化/格式化/超级块 A/B 回退/格式化中断
 *   - 流式写入、非对齐 chunk、去重、元数据、删除、first-fit 4KB 对齐
 *   - 写数据中断/写索引中断 → 不产生有效项
 *   - 写后 sha256 校验（故意篡改卡数据必须失败）
 *   - 索引 CRC 损坏、200 项扫描的扇区读次数、只写 /sb/ 区域
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "photosha.h"
#include "photostore.h"

#define SECTORS 4096          /* 2MB 测试卡 */
static uint8_t *g_disk;
static uint32_t g_writes, g_reads, g_max_write;
static long g_fail_writes = -1;     /* -1 不失败；0 起全部失败 */
static int g_fail_sector = -1;      /* >=0 时指定扇区写失败一次 */
static int g_fail_sector_hits;
static int g_fails;

static bool mem_read(void *ctx, uint32_t sec, void *buf, uint32_t nsec)
{
    (void)ctx;
    if (sec + nsec > SECTORS) return false;
    memcpy(buf, g_disk + (size_t)sec * 512, (size_t)nsec * 512);
    g_reads++;
    return true;
}

static bool mem_write(void *ctx, uint32_t sec, const void *buf, uint32_t nsec)
{
    (void)ctx;
    if (sec + nsec > SECTORS) return false;
    if (g_fail_sector >= 0 && (int)sec == g_fail_sector && g_fail_sector_hits < 1) {
        g_fail_sector_hits++;
        return false;
    }
    if (g_fail_writes == 0) return false;
    if (g_fail_writes > 0) g_fail_writes--;
    memcpy(g_disk + (size_t)sec * 512, buf, (size_t)nsec * 512);
    g_writes++;
    if (sec + nsec > g_max_write) g_max_write = sec + nsec;
    return true;
}

static photo_io_t IO = { .read = mem_read, .write = mem_write, .sector_count = SECTORS };

static void check(bool cond, const char *what)
{
    if (!cond) { printf("✗ %s\n", what); g_fails++; }
    else printf("✓ %s\n", what);
}

static void reset_card(void)
{
    memset(g_disk, 0xFF, (size_t)SECTORS * 512);
    g_writes = g_reads = g_max_write = 0;
    g_fail_writes = -1;
    g_fail_sector = -1;
    g_fail_sector_hits = 0;
}

static photostore_t open_store(uint8_t **buf)
{
    *buf = malloc(4096);
    photostore_t st;
    photostore_open(&st, &IO, *buf,
                    (*buf) ? 4096 : 0);
    return st;
}

static void fill(uint8_t *p, size_t n, uint32_t seed)
{
    for (size_t i = 0; i < n; i++) p[i] = (uint8_t)(seed + i * 31 + (i >> 8));
}

static bool put_photo(photostore_t *st, const uint8_t sha[32],
                      const uint8_t *data, uint32_t size, uint16_t dur,
                      uint16_t order)
{
    ps_put_t *p = NULL;
    bool exists = false;
    if (!photostore_put_begin(st, sha, size, dur, order, &p, &exists)) return false;
    if (exists) return true;
    /* 故意用不规则 chunk 写，覆盖 recv 分片场景 */
    uint32_t off = 0, step = 1;
    while (off < size) {
        uint32_t n = step;
        if (n > size - off) n = size - off;
        if (!photostore_put_write(p, data + off, n)) {
            photostore_put_abort(p);
            return false;
        }
        off += n;
        step = step * 7 % 3000 + 1;
    }
    return photostore_put_finish(p);
}

/* 回归：open() 传的是局部 photo_io_t。修复前 open 只存指针，函数返回后
 * 该栈帧被复用，format 会调到垃圾地址（上板表现为 InstrFetchProhibited,
 * PC=0）。这里在 open 之后把原表毒化，再 format/写入：只有按值拷贝才能过。 */
static bool local_io_lifetime(void)
{
    uint8_t *buf = malloc(4096);
    if (!buf) return false;
    photostore_t st;
    photo_io_t io = IO;          /* 局部变量，故意不留生命周期 */
    photostore_open(&st, &io, buf, 4096);
    bool uninit = photostore_state(&st) == PHOTO_UNINIT;

    io.read = NULL;              /* 毒化调用方的表 */
    io.write = NULL;
    bool fmt = photostore_format(&st, false);
    bool ready = photostore_state(&st) == PHOTO_READY;

    uint8_t data[700], h[32];
    fill(data, sizeof data, 77);
    photosha(data, sizeof data, h);
    bool put = put_photo(&st, h, data, sizeof data, 8, 0);

    free(buf);
    return uninit && fmt && ready && put;
}

int main(void)
{
    g_disk = malloc((size_t)SECTORS * 512);
    if (!g_disk) return 2;

    /* SHA-256 参考向量 */
    uint8_t d[32];
    photosha("abc", 3, d);
    static const uint8_t abc[32] = {
        0xba,0x78,0x16,0xbf,0x8f,0x01,0xcf,0xea,0x41,0x41,0x40,0xde,0x5d,0xae,0x22,0x23,
        0xb0,0x03,0x61,0xa3,0x96,0x17,0x7a,0x9c,0xb4,0x10,0xff,0x61,0xf2,0x00,0x15,0xad };
    check(memcmp(d, abc, 32) == 0, "SHA-256 参考向量");

    /* 1. 未初始化 → 不写；format 后可用 */
    reset_card();
    uint8_t *buf = NULL;
    photostore_t st = open_store(&buf);
    check(photostore_state(&st) == PHOTO_UNINIT, "空卡 = UNINIT");
    check(g_writes == 0, "UNINIT 不写卡");
    check(photostore_format(&st, false), "format 成功");
    check(photostore_state(&st) == PHOTO_READY, "format 后 READY");
    uint32_t free0 = 0, total = 0;
    uint16_t used0 = 0;
    check(photostore_space(&st, &free0, &total, &used0) && free0 == total,
          "空卡空闲 = 总量");

    /* 2. 写/读/去重/元数据/删除 */
    uint8_t data[10000], sha[32];
    fill(data, sizeof data, 1);
    photosha(data, sizeof data, sha);
    check(put_photo(&st, sha, data, sizeof data, 8, 3), "写入 10KB（不规则 chunk）");
    photo_file_t f;
    check(photostore_get(&st, sha, &f) && f.size == sizeof data, "get 成功");
    uint8_t *rb = malloc(sizeof data);
    int n = photostore_read(&st, &f, rb, sizeof data);
    check(n == (int)sizeof data && memcmp(rb, data, sizeof data) == 0, "读回字节一致");
    free(rb);
    ps_put_t *dup = NULL;
    bool exists = false;
    bool bret = photostore_put_begin(&st, sha, sizeof data, 8, 3, &dup, &exists);
    if (dup) photostore_put_abort(dup);
    check(bret && exists && !dup, "sha256 去重");
    check(photostore_update_meta(&st, sha, 9, 4), "更新 duration/order");
    check(photostore_walk(&st, NULL, NULL, NULL) == 1, "walk = 1");
    check(photostore_remove(&st, sha), "删除");
    check(photostore_walk(&st, NULL, NULL, NULL) == 0, "删除后 walk = 0");

    /* 3. 掉电：写数据中断 */
    reset_card();
    free(buf); st = open_store(&buf);
    photostore_format(&st, false);
    uint8_t big[8000], bsha[32];
    fill(big, sizeof big, 7);
    photosha(big, sizeof big, bsha);
    ps_put_t *p = NULL;
    bool ex = false;
    photostore_put_begin(&st, bsha, sizeof big, 8, 1, &p, &ex);
    g_fail_writes = 1;   /* 只允许第一块写成功，随后失败 */
    bool w1 = photostore_put_write(p, big, 4096);
    bool w2 = photostore_put_write(p, big + 4096, 4096);
    check(w1 && !w2, "写数据中途失败");
    photostore_put_abort(p);
    free(buf); st = open_store(&buf);
    check(photostore_get(&st, bsha, &f) == false, "数据中断后无有效项");

    /* 4. 掉电：写索引失败 */
    reset_card();
    free(buf); st = open_store(&buf);
    photostore_format(&st, false);
    fill(big, sizeof big, 9);
    photosha(big, sizeof big, bsha);
    g_fail_sector = PHOTO_INDEX_START;   /* 第一个索引扇区写失败 */
    bool ok = put_photo(&st, bsha, big, sizeof big, 8, 1);
    check(!ok, "索引提交失败 → put 返回失败");
    free(buf); st = open_store(&buf);
    check(photostore_get(&st, bsha, &f) == false, "索引未提交 → 不可见");
    g_fail_sector = -1;

    /* 5. 写后读回校验：卡数据被篡改必须拒绝提交 */
    reset_card();
    free(buf); st = open_store(&buf);
    photostore_format(&st, false);
    fill(big, sizeof big, 11);
    photosha(big, sizeof big, bsha);
    ps_put_t *p2 = NULL;
    photostore_put_begin(&st, bsha, sizeof big, 8, 1, &p2, &ex);
    photostore_put_write(p2, big, 4096);
    /* 篡改刚写的第一个数据扇区 */
    memset(g_disk + (size_t)st.lay.data_start * 512, 0xAA, 512);
    photostore_put_write(p2, big + 4096, sizeof big - 4096);
    check(!photostore_put_finish(p2), "写后读回 sha 不符 → 不提交");

    /* 6. A/B 超级块回退 */
    reset_card();
    free(buf); st = open_store(&buf);
    photostore_format(&st, false);      /* A seq2 */
    photostore_format(&st, false);      /* B seq4 */
    memset(g_disk + 512, 0x00, 512);   /* 破坏 B */
    free(buf); st = open_store(&buf);
    check(photostore_state(&st) == PHOTO_READY, "最新超级块损坏 → 回退旧 A/B");

    /* 7. 索引 CRC 损坏 → 忽略 */
    reset_card();
    free(buf); st = open_store(&buf);
    photostore_format(&st, false);
    fill(big, sizeof big, 13);
    photosha(big, sizeof big, bsha);
    put_photo(&st, bsha, big, sizeof big, 8, 1);
    g_disk[(size_t)PHOTO_INDEX_START * 512 + 60] ^= 0xFF;
    free(buf); st = open_store(&buf);
    check(photostore_get(&st, bsha, &f) == false, "索引 CRC 损坏 → 项不可用");

    /* 8. first-fit / 4KB 对齐 / 复用空洞 */
    reset_card();
    free(buf); st = open_store(&buf);
    photostore_format(&st, false);
    uint8_t a[4096], c[4096], as[32], cs[32];
    fill(a, sizeof a, 21); fill(c, sizeof c, 22);
    photosha(a, sizeof a, as); photosha(c, sizeof c, cs);
    put_photo(&st, as, a, sizeof a, 8, 0);
    put_photo(&st, cs, c, sizeof c, 8, 1);
    photo_file_t fa, fc;
    photostore_get(&st, as, &fa);
    photostore_get(&st, cs, &fc);
    check(fa.start_sector % 8 == 0 && fc.start_sector % 8 == 0, "4KB 对齐");
    check(fc.start_sector == fa.start_sector + 8, "连续分配");
    photostore_remove(&st, as);
    uint8_t d2[4096], ds[32];
    fill(d2, sizeof d2, 23); photosha(d2, sizeof d2, ds);
    put_photo(&st, ds, d2, sizeof d2, 8, 2);
    photo_file_t fd;
    photostore_get(&st, ds, &fd);
    check(fd.start_sector == fa.start_sector, "删除后 first-fit 复用空洞");

    /* 9. 200 项扫描的扇区读次数 */
    reset_card();
    free(buf); st = open_store(&buf);
    photostore_format(&st, false);
    uint8_t tiny[1024], tsha[32];
    fill(tiny, sizeof tiny, 31);
    for (int i = 0; i < 200; i++) {
        tiny[0] = (uint8_t)i;
        photosha(tiny, sizeof tiny, tsha);
        if (!put_photo(&st, tsha, tiny, sizeof tiny, 5, (uint16_t)i)) {
            check(false, "批量写入 200 项");
            break;
        }
    }
    g_reads = 0;
    uint32_t bad = 0;
    int valid = photostore_scan_index(&st, &bad);
    printf("  200 项 scan_index: valid=%d bad=%u 扇区读=%u\n", valid, bad, g_reads);
    check(valid == 200 && bad == 0, "scan_index 200 项");
    check(g_reads <= PHOTO_INDEX_SECTORS + 1, "扫描只读索引区（≤33 次扇区读）");

    /* 9b. format 模式：BLANK / CORRUPT / READY */
    reset_card();
    free(buf); st = open_store(&buf);
    check(photostore_format_mode(&st) == PHOTO_FMT_BLANK, "空卡 format_mode = BLANK");
    check(photostore_format(&st, false), "BLANK 无 force 可格式化");

    reset_card();
    memcpy(g_disk, PHOTO_SB_MAGIC, 4);
    memcpy(g_disk + 512, PHOTO_SB_MAGIC, 4);
    free(buf); st = open_store(&buf);
    check(photostore_state(&st) == PHOTO_FAULT, "魔数在但 CRC 全坏 = FAULT");
    check(photostore_format_mode(&st) == PHOTO_FMT_CORRUPT, "format_mode = CORRUPT");
    uint32_t w_before = g_writes;
    check(!photostore_format(&st, false), "CORRUPT 无 force 拒绝格式化");
    check(g_writes == w_before, "拒绝时不写卡");
    check(photostore_format(&st, true), "CORRUPT 带 force 格式化成功");
    check(photostore_state(&st) == PHOTO_READY, "force 格式化后 READY");

    check(photostore_format_mode(&st) == PHOTO_FMT_READY, "READY format_mode");
    w_before = g_writes;
    check(!photostore_format(&st, false) && !photostore_format(&st, true),
          "READY 状态一律拒绝格式化");
    check(g_writes == w_before, "READY 拒绝时不写卡");

    /* 10. format 掉电：超级块写失败 / 索引清零中断 */
    reset_card();
    free(buf); st = open_store(&buf);
    g_fail_sector = 0;                 /* 第一个超级块写失败 */
    check(!photostore_format(&st, false), "format 超级块写失败");
    free(buf); st = open_store(&buf);
    check(photostore_state(&st) == PHOTO_UNINIT, "format 中断后 = UNINIT");

    reset_card();
    free(buf); st = open_store(&buf);
    g_fail_sector = PHOTO_INDEX_START; /* 索引清零中途失败 */
    check(!photostore_format(&st, false), "format 索引清零中断");
    free(buf); st = open_store(&buf);
    check(photostore_state(&st) == PHOTO_UNINIT, "索引清零中断后 = UNINIT");
    check(photostore_format_mode(&st) == PHOTO_FMT_RESUME, "中断后可续做（RESUME）");
    check(photostore_format(&st, false), "RESUME 无 force 可续做");
    check(photostore_state(&st) == PHOTO_READY, "续做后 READY");

    /* 11. 只允许在 /sb/ 命名空间内写 */
    check(g_max_write <= SECTORS, "写不越过卡尾");
    photo_layout_t lay = st.lay;
    check(g_max_write <= lay.data_end, "写不越过 data_end");

    /* 12. open 的 IO 表生命周期（上板 PC=0 回归） */
    reset_card();
    check(local_io_lifetime(),
          "局部 IO 表：open 后毒化原表仍可 format/写入");

    free(buf);
    free(g_disk);
    if (g_fails) { printf("%d 项失败\n", g_fails); return 1; }
    printf("全部通过\n");
    return 0;
}
