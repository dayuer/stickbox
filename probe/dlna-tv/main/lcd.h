#pragma once
#include <stdbool.h>
#include <stdint.h>

/* 屏幕上显示的全部状态。main 实现 lcd_fill_status()，屏幕任务每 500ms 拉一次。 */
typedef enum {
    ST_WIFI,        /* 连 Wi-Fi 中 */
    ST_SEARCH,      /* 找电视中 */
    ST_NO_TV,       /* 没找到电视，退避中 */
    ST_PUSHED,      /* 已推送，等电视来拉流 */
    ST_STREAMING,   /* 电视正在拉流 */
    ST_PUSH_FAIL,   /* 推送被拒 */
} lcd_state_t;

typedef struct {
    lcd_state_t state;
    char ssid[33];
    char ip[16];
    char tv_name[48];
    char tv_ip[16];
    int page, pages;
    uint32_t frames;
    int retry_in_s;     /* NO_TV / PUSH_FAIL 时距下次重试的秒数 */
} lcd_status_t;

void lcd_fill_status(lcd_status_t *s);  /* 由 main 提供 */
void lcd_start(void);
