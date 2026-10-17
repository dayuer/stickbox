#pragma once
#include <stdbool.h>
#include <stdint.h>

/* 屏幕现在只做一件事：显示 Claude 订阅用量（数据由 usage_http.c 收进来）。
 * 下面这些旧接口还留着让业务代码照常调用，但除 lcd_ui_ip 外都不再上屏。 */
void lcd_ui_init(void);
void lcd_ui_wifi(const char *t);
void lcd_ui_ip(const char *t);         /* 页脚显示 IP：状态栏脚本要往这个地址 POST */
void lcd_ui_prn_name(const char *t);
void lcd_ui_prn(const char *t);
void lcd_ui_job(const char *t);
void lcd_ui_log(const char *line);
void lcd_ui_beat(void);

/* 一个限额窗口。字段对应 Claude Code statusline JSON 的 rate_limits.<窗口>。 */
typedef struct {
    bool    present;          /* 这次上报里有没有这个窗口 */
    float   used_pct;         /* used_percentage，0..100 */
    int64_t resets_at;        /* Unix 秒 */
} usage_window_t;

typedef struct {
    int64_t        now;       /* 发送方的 Unix 时间：板子没有 SNTP，靠它对时 */
    usage_window_t five_hour;
    usage_window_t seven_day;
    bool           has_cost;
    float          cost_usd;  /* 当前会话估算花费 cost.total_cost_usd */
    char           model[24];
} usage_report_t;

void lcd_ui_usage(const usage_report_t *u);
