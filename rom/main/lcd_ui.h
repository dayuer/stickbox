#pragma once
#include <stdbool.h>
#include <stdint.h>

/* 板载 240x240 状态屏：Wi-Fi / 电视 / 推流 / 云端 / 最近一次用量时间。
 * 所有 setter 都只拷贝字符串，调用方不必保活。 */
void lcd_ui_init(void);

void lcd_ui_wifi(const char *text);                 /* SSID 或「配网模式」 */
void lcd_ui_ip(const char *ip);
void lcd_ui_tv(const char *name, const char *ip);   /* name 为空 = 未找到 */
void lcd_ui_stream(const char *state, uint32_t frames);
void lcd_ui_cloud(const char *state);               /* 已连接 / 重连中 / 密钥被拒 */
void lcd_ui_log(const char *line);                  /* 底行提示（配网用） */

/* 底行显示配对码；paired=true 时显示「已配对」。 */
void lcd_ui_pair(const char *code, bool paired);

/* 一个限额窗口。字段对应服务端 usage.DevicePayload 的 five_hour / seven_day。 */
typedef struct {
    bool    present;
    float   used_pct;
    int64_t resets_at;      /* Unix 秒 */
} usage_window_t;

typedef struct {
    int64_t        now;     /* 服务端现场时间：设备没有 SNTP 时也可用它对表 */
    int64_t        ts;      /* 服务端收到数据的时间：「最近一次用量」显示它 */
    usage_window_t five_hour;
    usage_window_t seven_day;
} usage_report_t;

/* MQTT 收到一条用量后调用。屏幕只显示「最近一次用量时间」，
 * 窗口数据留给 T4 的电视看板。 */
void lcd_ui_usage(const usage_report_t *u);
