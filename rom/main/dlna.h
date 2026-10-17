#pragma once
#include <stdbool.h>

typedef struct {
    char name[64];        /* friendlyName */
    char ip[16];
    char control[160];    /* AVTransport 的完整 controlURL */
} dlna_tv_t;

/* SSDP 找一台带 AVTransport 的渲染器；manual_ip 非空则跳过发现（按三星固定路径） */
bool dlna_find_tv(const char *manual_ip, dlna_tv_t *out);

/* Stop → SetAVTransportURI → Play。图片/流在 SetURI 时就开始，Play 回 701 不算失败 */
bool dlna_push(const dlna_tv_t *tv, const char *url, const char *mime, const char *title);

bool dlna_stop(const dlna_tv_t *tv);
