/*
 * vdisk —— 合成式虚拟 U 盘（只读）。
 *
 * 不在 RAM 里放磁盘镜像：主机读哪个扇区，就现场生成那个扇区。
 * 这块板子只有 72~120KB 空闲堆，装不下任何真实大小的 FAT 卷；
 * 而一张 64MiB 的盘里 99.9% 的扇区是零，根本不需要存。
 *
 * 盘上内容来自编译进 flash 的 JPEG（EMBED_FILES，const，不占堆）。
 * 切换"当前幻灯片"会改变文件长度和 mtime —— 这正是我们要测的：
 * 电视会不会重读 FAT 和目录。
 */
#pragma once

#include <stdbool.h>
#include <stdint.h>

#define VD_SECTOR_SIZE 512u

/* ── 几何（见 vdisk.c 顶部的推导）── */
uint32_t vd_sector_count(void);

/* 生成 [lba, lba+nsec) 这些扇区到 dst。越界或空洞填零，不会失败。
 * 在 TinyUSB 的 MSC 回调里被调用，必须快且不阻塞。 */
void vd_read(uint32_t lba, uint8_t *dst, uint32_t nsec);

/* ── 幻灯片 ── */
int  vd_slide_count(void);
int  vd_slide_current(void);
int  vd_slide_pending(void);
uint32_t vd_slide_size(void);          /* 当前生效那张的字节数 */

/* 把 idx 设为"待生效"。不改变主机看到的内容。 */
void vd_slide_arm(int idx);

/* 让待生效的那张真正生效。应当在介质"不在位"的窗口里调用——
 * 文件长度一变，FAT 链长和目录项就都变了，正在读的主机会看到
 * 自相矛盾的卷。返回 true 表示确实发生了切换。 */
bool vd_slide_commit(void);

/* 不经过弹出直接换（故意不安全）：用来验证"电视会不会自己发现内容变了"。
 * 几乎可以肯定会看到旧缩略图或花屏，这是实验的一部分。 */
void vd_slide_commit_unsafe(int idx);

/* ── 读访问统计：诊断的主要产出 ──
 * 判断电视有没有重新扫描,看的就是弹出之后 fat/dir 计数有没有再涨。
 * 只涨 data 说明它在用缓存的目录结构。 */
typedef struct {
    uint32_t calls;        /* read10 回调次数 */
    uint32_t sectors;      /* 累计服务的扇区数 */
    uint32_t n_mbr;
    uint32_t n_bpb;        /* 含 FSInfo 与备份引导扇区 */
    uint32_t n_fat;
    uint32_t n_dir;        /* 根目录 + DCIM 目录 */
    uint32_t n_data;       /* 文件数据 */
    uint32_t n_hole;       /* 空洞（填零） */
    uint32_t last_lba;
    int64_t  first_us;     /* 本轮第一次读的时间戳，0=还没读过 */
    int64_t  last_us;
} vd_stats_t;

void vd_stats_get(vd_stats_t *out);
void vd_stats_reset(void);

/* 最近若干次读的人类可读摘要（按区域合并连续读），写入 buf。
 * 返回写入长度。这是用来看"电视到底按什么顺序读了什么"的。 */
int vd_trace(char *buf, int cap);
