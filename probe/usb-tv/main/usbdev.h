/*
 * USB 设备层：把 vdisk 以只读 MSC 暴露给电视，并提供三种"让主机重新扫盘"
 * 的手段供对比实验。
 */
#pragma once

#include <stdbool.h>
#include <stdint.h>

/* 初始化 USB PHY（device 模式）、装 TinyUSB、起 tud_task 任务。
 * 调用前必须先把板子切到 DEV 口（见 board.c）。 */
void usbdev_start(void);

bool usbdev_mounted(void);

/* ── 三种重扫手段，从轻到重。实验的核心就是比较它们在你那台电视上的效果。 ── */

/* ① 热换：直接改内容，不动 USB，不弹出。
 *    预期失败（电视用缓存的目录），但必须亲眼确认一次。 */
void usbdev_swap_hot(int slide);

/* ② SCSI 介质弹出：test_unit_ready 报 NOT_READY / MEDIUM NOT PRESENT，
 *    像读卡器被拔了卡。期间切换内容，然后恢复"在位"。
 *    USB 连接本身不断，对电视最温和。 */
void usbdev_eject_cycle(uint32_t absent_ms);

/* ③ USB 软拔插：tud_disconnect() / tud_connect()，整条总线重新枚举。
 *    最彻底。配合 usbdev_set_serial_varying(true) 还能让电视认为
 *    这是另一支从没见过的 U 盘。 */
void usbdev_replug(uint32_t down_ms);

/* 序列号是否每次枚举都变。很多主机按 (VID,PID,序列号) 缓存卷信息，
 * 变序列号 = 伪装成新设备,是 ③ 失效时的最后一招。 */
void usbdev_set_serial_varying(bool on);
bool usbdev_serial_varying(void);
const char *usbdev_serial(void);
