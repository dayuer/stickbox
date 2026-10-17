#pragma once
#include <stdint.h>

/* 状态文字的预渲染掩码。由 tools/gen_statusmask.py 生成，见那里的说明。
 *
 * 1-bit、按行打包，行宽向上取整到字节。设备侧只查位，不缩放、不排版。 */
typedef enum {
    STATUS_CONNECTING,   /* 正在连接… */
    STATUS_FETCHING,     /* 正在获取数据 */
    STATUS_OFFLINE,      /* 网络已断开 */
    STATUS_WAIT_PAIR,    /* 等待配对 */
    STATUS_NO_SERVER,    /* 连不上服务器 */
    STATUS_COUNT
} status_id_t;

typedef struct {
    const uint8_t *bits;
    uint16_t w, h, stride;
} status_mask_t;

extern const status_mask_t STATUS_MASKS[STATUS_COUNT];
