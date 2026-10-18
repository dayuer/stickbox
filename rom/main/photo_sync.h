/*
 * 照片同步（M2）：拉播放清单、初始化 SD、下载缺失照片、上报 status.photo。
 *
 * 不碰 stream.c / 帧订阅：清单和下载都在独立的低优先级任务里，和取帧
 * 通过 photo_io 互斥，取帧优先。
 */
#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "photo_json.h"

void photo_sync_init(void);

/* MQTT frame 通知里带 plv 时调用；plv 变化才会触发 HTTPS 清单拉取。 */
void photo_sync_notify_plv(int plv);

/* MQTT 连上、配对完成时调用：首次连接要主动拉一次清单。 */
void photo_sync_notify_connected(void);

/* 开机调一次：只挂卡、不格式化，让断网时也能从索引播放已缓存照片。
 * 由低优先级照片任务执行（SD 初始化需要 8KB 栈）。 */
void photo_sync_boot_mount(void);

/* 写 `"photo":{...}`（不含前导逗号），返回长度；失败 -1。
 * 心跳任务每 30 秒调用一次。 */
int photo_sync_status_json(char *out, size_t cap);

/* ---- M3 播放用 ---- */

/* 清单/索引每次变化（应用清单、下载完成、GC、格式化）递增；
 * photo_play 用它决定何时重建序列。 */
uint32_t photo_sync_play_gen(void);

/* 逐条访问当前清单，避免把 8KB 结构复制到调度任务栈上。
 * 回调参数：e==NULL 表示 board 元数据（board_s / has_board）；
 * 其余每个 photo 条目调用一次。返回 false 表示当前没有清单。 */
typedef void (*photo_sync_pl_visit_cb)(void *arg, const photo_pl_entry_t *e,
                                       uint16_t board_s, bool has_board);
bool photo_sync_playlist_visit(photo_sync_pl_visit_cb cb, void *arg);

/* 播放侧连续读失败 3 次后上报：进 status.photo.bad，并阻止再下载。 */
void photo_sync_report_bad(const uint8_t sha[32]);

/* 正在格式化（会清空整卡）：播放器应立刻切回 board 并停止发照片。 */
bool photo_sync_formatting(void);
