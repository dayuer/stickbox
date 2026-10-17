/*
 * 合成式只读虚拟 U 盘：带 MBR 的 64MiB FAT32 卷，内容现场生成。
 *
 * ── 几何推导（别改任何一个数字，除非重做这段算术）──
 *
 *   扇区 512B，整盘 131072 扇区 = 64 MiB
 *   LBA 0        MBR，一个分区，类型 0x0C (FAT32 LBA)
 *   LBA 2048     分区起点（真 U 盘的标准对齐），分区长 129024 扇区
 *
 *   卷内（相对分区起点）：
 *     0      BPB
 *     1      FSInfo
 *     6      BPB 备份
 *     7      FSInfo 备份
 *     32     FAT1，1008 扇区
 *     1040   FAT2，1008 扇区
 *     2048   数据区 = 簇 2
 *
 *   数据扇区 = 129024 - 32 - 2*1008 = 126976 → 126976 个簇（每簇 1 扇区）
 *   FAT 容量 = 1008 * 128 = 129024 项 ≥ 126978 ✓
 *   簇数 126976 ≥ 65525 ✓  —— 这条是 FAT32 的硬门槛，低于它
 *                             Windows/Linux 会按 FAT16 解析然后报错。
 *
 *   簇分配：
 *     2              根目录
 *     3              /DCIM 目录
 *     4    .. 2051   /SLIDE.JPG        （窗口 2048 簇 = 1 MiB 上限）
 *     2052 .. 4099   /DCIM/SLIDE.JPG   （同一份数据的第二条链）
 *
 *   两个副本各有独立的簇链，指向同一份 flash 里的 JPEG。
 *   这样文件系统完全合法（不共享簇），而内存开销是零。
 *   放两份是因为电视的照片浏览器扫根目录还是扫 DCIM 不确定——
 *   两边都放一份，哪边被读到会直接反映在 vd_trace 里。
 */

#include "vdisk.h"

#include <stdio.h>
#include <string.h>

#include "esp_log.h"
#include "esp_timer.h"

static const char *TAG = "vdisk";

/* ── 几何常量 ── */
#define DISK_SECTORS 131072u
#define PART_LBA     2048u
#define PART_SECTORS (DISK_SECTORS - PART_LBA) /* 129024 */

#define RSVD_SECTORS 32u
#define NUM_FATS     2u
#define SEC_PER_CLUS 1u
#define FAT_SECTORS  1008u

/* 绝对 LBA */
#define BPB_LBA   PART_LBA                            /* 2048 */
#define FAT1_LBA  (PART_LBA + RSVD_SECTORS)           /* 2080 */
#define FAT2_LBA  (FAT1_LBA + FAT_SECTORS)            /* 3088 */
#define DATA_LBA  (FAT2_LBA + FAT_SECTORS)            /* 4096 */
#define TOT_CLUS  (PART_SECTORS - RSVD_SECTORS - NUM_FATS * FAT_SECTORS) /* 126976 */

#define CL_ROOT      2u
#define CL_DCIM      3u
#define CL_F1        4u
#define FILE_MAX_CLUS 2048u /* 每份副本 1 MiB 上限 */
#define CL_F2        (CL_F1 + FILE_MAX_CLUS) /* 2052 */

#define CLUS_LBA(n)  (DATA_LBA + ((n) - 2u) * SEC_PER_CLUS)

#define FAT_EOC      0x0FFFFFFFu

/* ── 编译进 flash 的幻灯片（tools/make_slides.sh 生成）── */
#define SLIDE_SYM(n)                                      \
    extern const uint8_t _binary_slide##n##_jpg_start[];  \
    extern const uint8_t _binary_slide##n##_jpg_end[]
SLIDE_SYM(1);
SLIDE_SYM(2);
SLIDE_SYM(3);

typedef struct {
    const uint8_t *data;
    const uint8_t *end;
} slide_t;

static const slide_t s_slides[] = {
    { _binary_slide1_jpg_start, _binary_slide1_jpg_end },
    { _binary_slide2_jpg_start, _binary_slide2_jpg_end },
    { _binary_slide3_jpg_start, _binary_slide3_jpg_end },
};
#define NSLIDES ((int)(sizeof s_slides / sizeof s_slides[0]))

static int s_cur;      /* 主机当前看到的那张 */
static int s_armed;    /* 待生效 */
static int s_gen;      /* 每次生效 +1，用来变动文件 mtime */

static vd_stats_t s_st;

/* ── 读访问 trace 环 ── */
typedef enum { R_MBR, R_BPB, R_FAT, R_DIR, R_DATA, R_HOLE, R_N } region_t;
static const char *const REGION[R_N] = { "mbr", "bpb", "fat", "dir", "data", "hole" };

#define TRACE_N 48
typedef struct {
    uint8_t  region;
    uint32_t lba;
    uint32_t n;
    int64_t  us;
} trace_t;
static trace_t  s_tr[TRACE_N];
static uint32_t s_tr_head; /* 单调递增，取模索引 */

static void trace_put(region_t r, uint32_t lba, int64_t us)
{
    /* 跟上一条连续且同区域就合并，否则电视顺序读一个文件会把环刷爆 */
    if (s_tr_head) {
        trace_t *p = &s_tr[(s_tr_head - 1) % TRACE_N];
        if (p->region == r && p->lba + p->n == lba) {
            p->n++;
            p->us = us;
            return;
        }
    }
    trace_t *p = &s_tr[s_tr_head % TRACE_N];
    p->region = r;
    p->lba    = lba;
    p->n      = 1;
    p->us     = us;
    s_tr_head++;
}

/* ── 小端写入 ── */
static inline void put16(uint8_t *p, uint16_t v)
{
    p[0] = (uint8_t)v;
    p[1] = (uint8_t)(v >> 8);
}
static inline void put32(uint8_t *p, uint32_t v)
{
    p[0] = (uint8_t)v;
    p[1] = (uint8_t)(v >> 8);
    p[2] = (uint8_t)(v >> 16);
    p[3] = (uint8_t)(v >> 24);
}

/* ── FAT 日期时间 ── */
#define FAT_DATE(y, m, d)   ((uint16_t)((((y) - 1980) << 9) | ((m) << 5) | (d)))
#define FAT_TIME(h, mi, s)  ((uint16_t)(((h) << 11) | ((mi) << 5) | ((s) / 2)))

/* ── 幻灯片尺寸 ── */
static uint32_t slide_bytes(int idx)
{
    if (idx < 0 || idx >= NSLIDES) return 0;
    return (uint32_t)(s_slides[idx].end - s_slides[idx].data);
}

static uint32_t slide_clusters(int idx)
{
    uint32_t n = (slide_bytes(idx) + VD_SECTOR_SIZE - 1) / VD_SECTOR_SIZE;
    if (n == 0) n = 1;                      /* 空文件也占一簇，省掉特例 */
    if (n > FILE_MAX_CLUS) n = FILE_MAX_CLUS;
    return n;
}

/* ── MBR ── */
static void chs(uint8_t *out, uint32_t lba)
{
    /* 经典 63 扇区/磁道、255 磁头。超出可表示范围就写 0xFE/0xFF/0xFF。 */
    const uint32_t spt = 63, hpc = 255;
    uint32_t c = lba / (spt * hpc);
    uint32_t h = (lba / spt) % hpc;
    uint32_t s = lba % spt + 1;
    if (c > 1023) {
        out[0] = 0xFE;
        out[1] = 0xFF;
        out[2] = 0xFF;
        return;
    }
    out[0] = (uint8_t)h;
    out[1] = (uint8_t)(s | ((c >> 2) & 0xC0));
    out[2] = (uint8_t)c;
}

static void gen_mbr(uint8_t *b)
{
    uint8_t *e = b + 0x1BE;
    e[0] = 0x00;                     /* 非活动分区；电视不从这儿引导 */
    chs(&e[1], PART_LBA);
    e[4] = 0x0C;                     /* FAT32 LBA */
    chs(&e[5], DISK_SECTORS - 1);
    put32(&e[8], PART_LBA);
    put32(&e[12], PART_SECTORS);
    b[510] = 0x55;
    b[511] = 0xAA;
}

/* ── BPB ── */
static void gen_bpb(uint8_t *b)
{
    b[0] = 0xEB;
    b[1] = 0x58;
    b[2] = 0x90;
    memcpy(&b[3], "MSWIN4.1", 8);    /* 最被广泛接受的 OEM 串 */
    put16(&b[11], VD_SECTOR_SIZE);
    b[13] = SEC_PER_CLUS;
    put16(&b[14], RSVD_SECTORS);
    b[16] = NUM_FATS;
    put16(&b[17], 0);                /* FAT32：根目录项数必须为 0 */
    put16(&b[19], 0);                /* TotSec16 为 0，用 TotSec32 */
    b[21] = 0xF8;                    /* 固定介质 */
    put16(&b[22], 0);                /* FATSz16 为 0 */
    put16(&b[24], 63);               /* 扇区/磁道 */
    put16(&b[26], 255);              /* 磁头数 */
    put32(&b[28], PART_LBA);         /* 隐藏扇区 = 分区前的扇区数 */
    put32(&b[32], PART_SECTORS);
    put32(&b[36], FAT_SECTORS);
    put16(&b[40], 0);                /* ExtFlags：两个 FAT 都镜像 */
    put16(&b[42], 0);                /* FSVer 0.0 */
    put32(&b[44], CL_ROOT);
    put16(&b[48], 1);                /* FSInfo 在卷内第 1 扇区 */
    put16(&b[50], 6);                /* 备份引导扇区在卷内第 6 扇区 */
    b[64] = 0x80;                    /* 驱动器号 */
    b[66] = 0x29;                    /* 扩展引导签名 */
    put32(&b[67], 0x455350B2u);      /* 卷序列号，随便但固定 */
    memcpy(&b[71], "ESP32 TV   ", 11);
    memcpy(&b[82], "FAT32   ", 8);
    b[510] = 0x55;
    b[511] = 0xAA;
}

static void gen_fsinfo(uint8_t *b)
{
    put32(&b[0], 0x41615252u);
    put32(&b[484], 0x61417272u);
    put32(&b[488], 0xFFFFFFFFu);     /* 空闲簇数未知——不如实报，别撒谎 */
    put32(&b[492], 0xFFFFFFFFu);     /* 下一个空闲簇未知 */
    put32(&b[508], 0xAA550000u);
    b[510] = 0x55;
    b[511] = 0xAA;
}

/* ── FAT ── */
static uint32_t fat_entry(uint32_t n, int cur)
{
    if (n == 0) return 0x0FFFFFF8u;  /* 介质字节 */
    if (n == 1) return FAT_EOC;
    if (n == CL_ROOT || n == CL_DCIM) return FAT_EOC;

    uint32_t len = slide_clusters(cur);
    if (n >= CL_F1 && n < CL_F1 + len) return (n == CL_F1 + len - 1) ? FAT_EOC : n + 1;
    if (n >= CL_F2 && n < CL_F2 + len) return (n == CL_F2 + len - 1) ? FAT_EOC : n + 1;
    return 0;                        /* 空闲 */
}

static void gen_fat(uint8_t *b, uint32_t fat_sec, int cur)
{
    const uint32_t per = VD_SECTOR_SIZE / 4; /* 128 项/扇区 */
    uint32_t base = fat_sec * per;
    for (uint32_t i = 0; i < per; i++) {
        uint32_t n = base + i;
        if (n >= TOT_CLUS + 2) break;
        put32(&b[i * 4], fat_entry(n, cur));
    }
}

/* ── 目录项 ── */
static void dirent(uint8_t *e, const char *name83, uint8_t attr, uint32_t clus,
                   uint32_t size, uint16_t date, uint16_t time)
{
    memcpy(e, name83, 11);
    e[11] = attr;
    put16(&e[14], time);             /* 创建时间 */
    put16(&e[16], date);             /* 创建日期 */
    put16(&e[18], date);             /* 访问日期 */
    put16(&e[20], (uint16_t)(clus >> 16));
    put16(&e[22], time);             /* 修改时间 */
    put16(&e[24], date);             /* 修改日期 */
    put16(&e[26], (uint16_t)(clus & 0xFFFF));
    put32(&e[28], size);
}

static void gen_root(uint8_t *b, int cur, int gen)
{
    uint16_t d = FAT_DATE(2026, 10, 17);
    uint16_t t = FAT_TIME(12, (unsigned)gen % 60u, 0); /* 每次切换 mtime 都变 */
    dirent(b + 0,  "ESP32 TV   ", 0x08, 0,      0, d, t);
    dirent(b + 32, "SLIDE   JPG", 0x20, CL_F1, slide_bytes(cur), d, t);
    dirent(b + 64, "DCIM       ", 0x10, CL_DCIM, 0, d, t);
}

static void gen_dcim(uint8_t *b, int cur, int gen)
{
    uint16_t d = FAT_DATE(2026, 10, 17);
    uint16_t t = FAT_TIME(12, (unsigned)gen % 60u, 0);
    dirent(b + 0,  ".          ", 0x10, CL_DCIM, 0, d, t);
    dirent(b + 32, "..         ", 0x10, 0,       0, d, t); /* FAT32：父为根时写 0 */
    dirent(b + 64, "SLIDE   JPG", 0x20, CL_F2, slide_bytes(cur), d, t);
}

/* ── 单扇区分发 ── */
static region_t gen_sector(uint32_t lba, uint8_t *b, int cur, int gen)
{
    memset(b, 0, VD_SECTOR_SIZE);

    if (lba == 0) {
        gen_mbr(b);
        return R_MBR;
    }
    if (lba == BPB_LBA || lba == BPB_LBA + 6) {
        gen_bpb(b);
        return R_BPB;
    }
    if (lba == BPB_LBA + 1 || lba == BPB_LBA + 7) {
        gen_fsinfo(b);
        return R_BPB;
    }
    if (lba >= FAT1_LBA && lba < FAT1_LBA + FAT_SECTORS) {
        gen_fat(b, lba - FAT1_LBA, cur);
        return R_FAT;
    }
    if (lba >= FAT2_LBA && lba < FAT2_LBA + FAT_SECTORS) {
        gen_fat(b, lba - FAT2_LBA, cur);
        return R_FAT;
    }
    if (lba == CLUS_LBA(CL_ROOT)) {
        gen_root(b, cur, gen);
        return R_DIR;
    }
    if (lba == CLUS_LBA(CL_DCIM)) {
        gen_dcim(b, cur, gen);
        return R_DIR;
    }

    /* 文件数据：两条链指向同一份 flash 数据 */
    const uint32_t f1 = CLUS_LBA(CL_F1), f2 = CLUS_LBA(CL_F2);
    uint32_t off = 0;
    bool hit = false;
    if (lba >= f1 && lba < f1 + FILE_MAX_CLUS) {
        off = (lba - f1) * VD_SECTOR_SIZE;
        hit = true;
    } else if (lba >= f2 && lba < f2 + FILE_MAX_CLUS) {
        off = (lba - f2) * VD_SECTOR_SIZE;
        hit = true;
    }
    if (hit) {
        uint32_t size = slide_bytes(cur);
        if (off < size) {
            uint32_t n = size - off;
            if (n > VD_SECTOR_SIZE) n = VD_SECTOR_SIZE;
            memcpy(b, s_slides[cur].data + off, n); /* flash 映射，直接读 */
            return R_DATA;
        }
        return R_DATA;                              /* 文件尾部的零填充 */
    }
    return R_HOLE;
}

/* ── 对外接口 ── */
uint32_t vd_sector_count(void) { return DISK_SECTORS; }

void vd_read(uint32_t lba, uint8_t *dst, uint32_t nsec)
{
    /* 一次调用内快照，保证主机看到的这一批扇区自相一致。
     * 幻灯片表是 const，索引快照就够了，不需要锁。 */
    const int cur = s_cur, gen = s_gen;
    const int64_t now = esp_timer_get_time();

    if (!s_st.first_us) s_st.first_us = now;
    s_st.last_us = now;
    s_st.calls++;

    for (uint32_t i = 0; i < nsec; i++) {
        uint32_t l = lba + i;
        uint8_t *b = dst + i * VD_SECTOR_SIZE;
        if (l >= DISK_SECTORS) {
            memset(b, 0, VD_SECTOR_SIZE);
            continue;
        }
        region_t r = gen_sector(l, b, cur, gen);
        s_st.sectors++;
        s_st.last_lba = l;
        switch (r) {
        case R_MBR:  s_st.n_mbr++;  break;
        case R_BPB:  s_st.n_bpb++;  break;
        case R_FAT:  s_st.n_fat++;  break;
        case R_DIR:  s_st.n_dir++;  break;
        case R_DATA: s_st.n_data++; break;
        default:     s_st.n_hole++; break;
        }
        trace_put(r, l, now);
    }
}

int vd_slide_count(void)   { return NSLIDES; }
int vd_slide_current(void) { return s_cur; }
int vd_slide_pending(void) { return s_armed; }
uint32_t vd_slide_size(void) { return slide_bytes(s_cur); }

void vd_slide_arm(int idx)
{
    if (idx < 0 || idx >= NSLIDES) return;
    s_armed = idx;
    ESP_LOGI(TAG, "待生效 -> slide%d (%u 字节)", idx + 1, (unsigned)slide_bytes(idx));
}

bool vd_slide_commit(void)
{
    if (s_armed == s_cur) return false;
    s_cur = s_armed;
    s_gen++;
    ESP_LOGI(TAG, "已生效 slide%d，%u 字节，%u 簇，gen=%d", s_cur + 1,
             (unsigned)slide_bytes(s_cur), (unsigned)slide_clusters(s_cur), s_gen);
    return true;
}

void vd_slide_commit_unsafe(int idx)
{
    if (idx < 0 || idx >= NSLIDES) return;
    s_armed = idx;
    s_cur   = idx;
    s_gen++;
    ESP_LOGW(TAG, "热切换到 slide%d（未弹出，主机可能看到不一致的卷）", idx + 1);
}

void vd_stats_get(vd_stats_t *out) { *out = s_st; }

void vd_stats_reset(void)
{
    memset(&s_st, 0, sizeof s_st);
    s_tr_head = 0;
    memset(s_tr, 0, sizeof s_tr);
}

int vd_trace(char *buf, int cap)
{
    int n = 0;
    uint32_t total = s_tr_head;
    uint32_t start = total > TRACE_N ? total - TRACE_N : 0;
    int64_t base = s_st.first_us;
    for (uint32_t i = start; i < total && n < cap - 1; i++) {
        const trace_t *p = &s_tr[i % TRACE_N];
        n += snprintf(buf + n, cap - n, "%8lldms %-4s lba=%-6u x%u\n",
                      (long long)((p->us - base) / 1000), REGION[p->region],
                      (unsigned)p->lba, (unsigned)p->n);
    }
    if (n < cap) buf[n] = 0;
    return n;
}
