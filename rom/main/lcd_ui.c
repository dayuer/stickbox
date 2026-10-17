/*
 * 板载 ST7789（240x240）状态屏：Wi-Fi / 电视 / 推流 / 云端 / 最近一次用量时间。
 *
 * 显示初始化沿用原主工程：BSP 不交出 panel 句柄，没法设镜像，所以把
 * bsp_display_new + lvgl_port_add_disp 拆开自己接。实机底部约 12px 被
 * 屏幕边框挡住，内容全部压在 y<224。
 *
 * 所有状态由其他任务通过 setter 写入，渲染任务每 500ms 取一份快照。
 * 不在网络任务里直接碰 LVGL：渲染中文很吃栈，以前在回调里直接调爆过栈。
 */
#include <stdio.h>
#include <string.h>
#include <time.h>

#include "bsp/esp-bsp.h"
#include "esp_lcd_panel_ops.h"
#include "esp_log.h"
#include "esp_lvgl_port.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "lvgl.h"
#include "sdkconfig.h"

#include "lcd_ui.h"

LV_FONT_DECLARE(font_cn_16);

static const char *TAG = "lcd_ui";

#define C_BG     0x0B1220
#define C_TEXT   0xE5E7EB
#define C_MUTED  0x8B95A7
#define C_LINE   0x253047
#define C_OK     0x4ADE80
#define C_WAIT   0xFBBF24
#define C_BAD    0xF87171
#define C_INFO   0x60A5FA

typedef struct {
    char     wifi[40];
    char     ip[20];
    char     tv[48];
    char     tv_ip[20];
    char     stream[32];
    char     cloud[32];
    char     hint[64];
    uint32_t frames;
    int      stream_color;
    bool     have_usage;
    int64_t  usage_ts;       /* 服务端收到数据的时间 */
    int64_t  usage_rx_us;    /* 本机收到它的时刻，SNTP 未对时时用它算相对时间 */
} ui_state_t;

static ui_state_t        s_st;
static SemaphoreHandle_t s_mx;

static lv_obj_t *s_wifi, *s_ip, *s_tv, *s_tvip, *s_dot, *s_stream, *s_frames;
static lv_obj_t *s_cloud, *s_usage, *s_hint;

static bool display_start(void)
{
    const lvgl_port_cfg_t pcfg = ESP_LVGL_PORT_INIT_CONFIG();
    if (lvgl_port_init(&pcfg) != ESP_OK) return false;

    esp_lcd_panel_handle_t panel = NULL;
    esp_lcd_panel_io_handle_t io = NULL;
    const bsp_display_config_t bcfg = {
        .max_transfer_sz = BSP_LCD_DRAW_BUFF_SIZE * sizeof(uint16_t),
    };
    if (bsp_display_new(&bcfg, &panel, &io) != ESP_OK) return false;

#if CONFIG_STICKBOX_LCD_ROTATE_180
    const bool flip = true;
    esp_lcd_panel_mirror(panel, true, true);
    esp_lcd_panel_set_gap(panel, 0, 320 - BSP_LCD_V_RES); /* 显存 240x320 只接了前 240 行 */
#else
    const bool flip = false;
#endif
    esp_lcd_panel_disp_on_off(panel, true);

    const lvgl_port_display_cfg_t dcfg = {
        .io_handle = io, .panel_handle = panel,
        .buffer_size = BSP_LCD_H_RES * 16, .double_buffer = false,
        .hres = BSP_LCD_H_RES, .vres = BSP_LCD_V_RES, .monochrome = false,
        .rotation = { .swap_xy = false, .mirror_x = flip, .mirror_y = flip },
        .flags = { .buff_dma = true, .buff_spiram = false,
                   .swap_bytes = BSP_LCD_BIGENDIAN ? true : false },
    };
    return lvgl_port_add_disp(&dcfg) != NULL;
}

static lv_obj_t *label(int x, int y, int w, uint32_t color, lv_text_align_t al)
{
    lv_obj_t *l = lv_label_create(lv_scr_act());
    lv_obj_set_pos(l, x, y);
    if (w) lv_obj_set_width(l, w);
    lv_label_set_long_mode(l, LV_LABEL_LONG_MODE_DOTS);
    lv_obj_set_style_text_color(l, lv_color_hex(color), 0);
    lv_obj_set_style_text_font(l, &font_cn_16, 0);
    lv_obj_set_style_text_align(l, al, 0);
    lv_label_set_text(l, "");
    return l;
}

static void build(void)
{
    lv_obj_set_style_bg_color(lv_scr_act(), lv_color_hex(C_BG), 0);

    lv_obj_t *t = label(14, 4, 212, C_MUTED, LV_TEXT_ALIGN_LEFT);
    lv_label_set_text(t, "StickBox 状态");

    s_wifi = label(14, 26, 212, C_TEXT, LV_TEXT_ALIGN_LEFT);
    s_ip   = label(14, 46, 212, C_MUTED, LV_TEXT_ALIGN_LEFT);

    lv_obj_t *ln = lv_obj_create(lv_scr_act());
    lv_obj_remove_style_all(ln);
    lv_obj_set_pos(ln, 14, 68);
    lv_obj_set_size(ln, 212, 1);
    lv_obj_set_style_bg_opa(ln, LV_OPA_COVER, 0);
    lv_obj_set_style_bg_color(ln, lv_color_hex(C_LINE), 0);

    s_tv   = label(14, 76, 212, C_TEXT, LV_TEXT_ALIGN_LEFT);
    s_tvip = label(14, 96, 212, C_MUTED, LV_TEXT_ALIGN_LEFT);

    s_dot = lv_obj_create(lv_scr_act());
    lv_obj_remove_style_all(s_dot);
    lv_obj_set_pos(s_dot, 14, 121);
    lv_obj_set_size(s_dot, 10, 10);
    lv_obj_set_style_radius(s_dot, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_opa(s_dot, LV_OPA_COVER, 0);
    s_stream = label(32, 116, 194, C_TEXT, LV_TEXT_ALIGN_LEFT);
    s_frames = label(32, 138, 194, C_MUTED, LV_TEXT_ALIGN_LEFT);

    s_cloud = label(14, 160, 212, C_TEXT, LV_TEXT_ALIGN_LEFT);
    s_usage = label(14, 182, 212, C_MUTED, LV_TEXT_ALIGN_LEFT);
    s_hint  = label(14, 204, 212, C_MUTED, LV_TEXT_ALIGN_LEFT);
}

/* 用量时间：SNTP 对上时显示服务端收到数据的本地 HH:MM，否则显示相对时间。 */
static void fmt_usage(char *b, size_t cap, const ui_state_t *s)
{
    if (!s->have_usage) {
        snprintf(b, cap, "最近用量  —");
        return;
    }
    time_t now = time(NULL);
    struct tm tm;
    localtime_r(&now, &tm);
    if (tm.tm_year >= 2025 - 1900) {
        time_t t = (time_t)s->usage_ts;
        struct tm ut;
        localtime_r(&t, &ut);
        snprintf(b, cap, "最近用量  %02d:%02d", ut.tm_hour, ut.tm_min);
        return;
    }
    int age = (int)((esp_timer_get_time() - s->usage_rx_us) / 1000000);
    if (age < 60)       snprintf(b, cap, "最近用量  %d 秒前", age);
    else if (age < 3600) snprintf(b, cap, "最近用量  %d 分前", age / 60);
    else                 snprintf(b, cap, "最近用量  %d 时前", age / 3600);
}

static void render(const ui_state_t *s)
{
    char b[96];

    snprintf(b, sizeof b, "Wi-Fi  %s", s->wifi[0] ? s->wifi : "—");
    lv_label_set_text(s_wifi, b);
    lv_label_set_text(s_ip, s->ip);

    if (s->tv[0]) {
        snprintf(b, sizeof b, "电视  %s", s->tv);
        lv_label_set_text(s_tv, b);
        lv_label_set_text(s_tvip, s->tv_ip);
    } else {
        lv_label_set_text(s_tv, "电视  —");
        lv_label_set_text(s_tvip, "");
    }

    lv_obj_set_style_bg_color(s_dot, lv_color_hex(s->stream_color), 0);
    lv_obj_set_style_text_color(s_stream, lv_color_hex(s->stream_color), 0);
    lv_label_set_text(s_stream, s->stream[0] ? s->stream : "推流  等待中");
    if (s->frames) snprintf(b, sizeof b, "已发 %lu 帧", (unsigned long)s->frames);
    else b[0] = 0;
    lv_label_set_text(s_frames, b);

    snprintf(b, sizeof b, "云端  %s", s->cloud[0] ? s->cloud : "—");
    lv_label_set_text(s_cloud, b);

    fmt_usage(b, sizeof b, s);
    lv_label_set_text(s_usage, b);

    lv_label_set_text(s_hint, s->hint);
}

static void lcd_task(void *arg)
{
    (void)arg;
    ui_state_t copy;
    while (1) {
        xSemaphoreTake(s_mx, portMAX_DELAY);
        copy = s_st;
        xSemaphoreGive(s_mx);
        if (bsp_display_lock(100)) {
            render(&copy);
            bsp_display_unlock();
        }
        vTaskDelay(pdMS_TO_TICKS(500));
    }
}

void lcd_ui_init(void)
{
    if (!display_start()) {
        ESP_LOGE(TAG, "屏幕初始化失败——不影响推流");
        return;
    }
    s_mx = xSemaphoreCreateMutex();
    bsp_display_brightness_set(80);
    bsp_display_lock(0);
    build();
    bsp_display_unlock();
    xTaskCreate(lcd_task, "lcd_ui", 4096, NULL, 2, NULL);
}

/* ── setter：只改状态，渲染任务负责上屏 ── */
static void set_str(char *dst, size_t cap, const char *src)
{
    if (!s_mx) return;
    xSemaphoreTake(s_mx, portMAX_DELAY);
    snprintf(dst, cap, "%s", src ? src : "");
    xSemaphoreGive(s_mx);
}

void lcd_ui_wifi(const char *text) { set_str(s_st.wifi, sizeof s_st.wifi, text); }
void lcd_ui_ip(const char *ip)     { set_str(s_st.ip, sizeof s_st.ip, ip); }
void lcd_ui_cloud(const char *s)   { set_str(s_st.cloud, sizeof s_st.cloud, s); }
void lcd_ui_log(const char *line)  { set_str(s_st.hint, sizeof s_st.hint, line); }

void lcd_ui_pair(const char *code, bool paired)
{
    char b[32];
    if (paired)                 snprintf(b, sizeof b, "已配对");
    else if (code && code[0])   snprintf(b, sizeof b, "配对码 %s", code);
    else                        b[0] = 0;
    set_str(s_st.hint, sizeof s_st.hint, b);
}

void lcd_ui_tv(const char *name, const char *ip)
{
    if (!s_mx) return;
    xSemaphoreTake(s_mx, portMAX_DELAY);
    snprintf(s_st.tv, sizeof s_st.tv, "%s", name ? name : "");
    snprintf(s_st.tv_ip, sizeof s_st.tv_ip, "%s", ip ? ip : "");
    xSemaphoreGive(s_mx);
}

void lcd_ui_stream(const char *state, uint32_t frames)
{
    if (!s_mx) return;
    int color = C_INFO;
    if (state) {
        if (strstr(state, "推流中"))                 color = C_OK;
        else if (strstr(state, "未找到") || strstr(state, "失败")) color = C_BAD;
        else if (strstr(state, "推送") || strstr(state, "等电视")) color = C_WAIT;
    }
    xSemaphoreTake(s_mx, portMAX_DELAY);
    snprintf(s_st.stream, sizeof s_st.stream, "%s", state ? state : "");
    s_st.frames = frames;
    s_st.stream_color = color;
    xSemaphoreGive(s_mx);
}

void lcd_ui_usage(const usage_report_t *u)
{
    if (!u) return;
    if (!s_mx) {
        /* 屏幕没起来也要把解析结果打到串口，上板验证靠它。 */
        ESP_LOGI(TAG, "用量：5h=%.1f%% reset=%lld  7d=%.1f%% reset=%lld  ts=%lld",
                 u->five_hour.used_pct, (long long)u->five_hour.resets_at,
                 u->seven_day.used_pct, (long long)u->seven_day.resets_at,
                 (long long)u->ts);
        return;
    }
    xSemaphoreTake(s_mx, portMAX_DELAY);
    s_st.have_usage = true;
    s_st.usage_ts = u->ts ? u->ts : u->now;
    s_st.usage_rx_us = esp_timer_get_time();
    xSemaphoreGive(s_mx);

    ESP_LOGI(TAG, "用量：5h=%.1f%% reset=%lld  7d=%.1f%% reset=%lld  now=%lld ts=%lld",
             u->five_hour.used_pct, (long long)u->five_hour.resets_at,
             u->seven_day.used_pct, (long long)u->seven_day.resets_at,
             (long long)u->now, (long long)u->ts);
}
