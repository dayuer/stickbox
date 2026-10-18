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

void photo_sync_init(void);

/* MQTT frame 通知里带 plv 时调用；plv 变化才会触发 HTTPS 清单拉取。 */
void photo_sync_notify_plv(int plv);

/* MQTT 连上、配对完成时调用：首次连接要主动拉一次清单。 */
void photo_sync_notify_connected(void);

/* 写 `"photo":{...}`（不含前导逗号），返回长度；失败 -1。
 * 心跳任务每 30 秒调用一次。 */
int photo_sync_status_json(char *out, size_t cap);
