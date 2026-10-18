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

/* M3 播放器直接读这份唯一的清单（不再复制一份序列）。
 * 先 online 判断来源；lock 拿 s_pl 的读锁，回调/使用期间不能解锁。
 * online=true：服务端清单，数组顺序权威；false：由 SD 索引构建的离线清单。 */
bool photo_sync_playlist_online(void);
bool photo_sync_playlist_lock(const photo_pl_t **pl, bool *online,
                              uint32_t timeout_ms);
void photo_sync_playlist_unlock(void);

/* 没有在线清单时，用 SD 索引填 s_pl（board 默认 20s）；返回照片数。
 * 调用方通常是播放调度任务（栈 4KB），walk 在这里完成。 */
int photo_sync_build_offline(void);

/* 播放侧连续读失败 3 次后上报：进 status.photo.bad，并阻止再下载。 */
void photo_sync_report_bad(const uint8_t sha[32]);

/* 正在格式化（会清空整卡）：播放器应立刻切回 board 并停止发照片。 */
bool photo_sync_formatting(void);
