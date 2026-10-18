#pragma once
#include <stdbool.h>
#include <stdint.h>

#include "photostore.h"

/* microSD 适配层（M1）：按需挂载、裸扇区 IO、状态机。
 *
 * 不自动格式化：只有显式调用 photo_sd_format() 才会写卡。M1 只接收
 * authorized/force 两个标志；init_token 的校验与 10 分钟有效期在 M2 的
 * 清单链路里完成，过了校验才把 authorized 置位。
 *
 * 挂载/读卡期间调用方必须保证不与取帧 TLS 并发（M2/M3 负责互斥）。 */

photo_state_t photo_sd_mount(void);   /* 按需挂载；失败返回 ABSENT/FAULT */
void photo_sd_unmount(void);          /* 释放 SDMMC 与缓冲 */
bool photo_sd_mounted(void);

photo_state_t photo_sd_state(void);   /* 当前状态（未挂载 = ABSENT） */

/* authorized=false 直接拒绝；force 只用于“魔数存在但 CRC 全坏”。
 * READY/UNAVAILABLE 一律拒绝。返回 true 表示格式化完成。 */
bool photo_sd_format(bool authorized, bool force);

/* 已挂载时返回 BLANK/RESUME/CORRUPT/READY；未挂载返回 UNAVAILABLE。 */
photo_format_mode_t photo_sd_format_mode(void);

/* 挂载且 READY 时返回存储对象，否则 NULL。 */
photostore_t *photo_sd_store(void);
