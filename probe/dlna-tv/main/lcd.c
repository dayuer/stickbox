/*
 * 板载 ST7789（240x240）状态屏：Wi-Fi / 电视 / 推流状态 / 当前页。
 * 显示初始化照搬主工程 main/lcd_ui.c 的 display_start()（BSP 不交出 panel 句柄，没法设镜像）。
 * 字体：font_cn_16 是主工程那份 GB2312 一级字 + ASCII 子集，界面文字只用其中的字。
 *
 * 实机底部约 12px 被屏幕边框挡住（主工程 2026-10-17 实拍），内容压在 y<224 以内。
 */
#include "lcd.h"

#include <stdio.h>
#include <string.h>

#include "bsp/esp-bsp.h"
#include "esp_lcd_panel_ops.h"
#include "esp_log.h"
#include "esp_lvgl_port.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lvgl.h"

static const char *TAG = "lcd";
LV_FONT_DECLARE(font_cn_16);

#define C_BG     0x0B1220
#define C_TEXT   0xE5E7EB
#define C_MUTED  0x8B95A7
#define C_LINE   0x253047
#define C_OK     0x4ADE80
#define C_WAIT   0xFBBF24
#define C_BAD    0xF87171
#define C_INFO   0x60A5FA

static lv_obj_t *s_wifi, *s_ip, *s_tv, *s_tvip, *s_dot, *s_state, *s_sub, *s_page, *s_pages;

static bool display_start(void)
{
    const lvgl_port_cfg_t pcfg = ESP_LVGL_PORT_INIT_CONFIG();
    if (lvgl_port_init(&pcfg) != ESP_OK) return false;

    esp_lcd_panel_handle_t panel = NULL;
    esp_lcd_panel_io_handle_t io = NULL;
    const bsp_display_config_t bcfg = { .max_transfer_sz = BSP_LCD_DRAW_BUFF_SIZE * sizeof(uint16_t) };
    if (bsp_display_new(&bcfg, &panel, &io) != ESP_OK) return false;

#if CONFIG_DLNA_LCD_ROTATE_180
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

static lv_obj_t *label(int x, int y, int w, uint32_t color, const lv_font_t *f, lv_text_align_t al)
{
    lv_obj_t *l = lv_label_create(lv_scr_act());
    lv_obj_set_pos(l, x, y);
    if (w) lv_obj_set_width(l, w);
    lv_label_set_long_mode(l, LV_LABEL_LONG_MODE_DOTS);
    lv_obj_set_style_text_color(l, lv_color_hex(color), 0);
    lv_obj_set_style_text_font(l, f, 0);
    lv_obj_set_style_text_align(l, al, 0);
    lv_label_set_text(l, "");
    return l;
}

static void build(void)
{
    lv_obj_set_style_bg_color(lv_scr_act(), lv_color_hex(C_BG), 0);

    lv_obj_t *t = label(14, 10, 212, C_MUTED, &font_cn_16, LV_TEXT_ALIGN_LEFT);
    lv_label_set_text(t, "电视推流");
    s_wifi = label(14, 34, 212, C_TEXT, &font_cn_16, LV_TEXT_ALIGN_LEFT);
    s_ip   = label(14, 54, 212, C_MUTED, &font_cn_16, LV_TEXT_ALIGN_LEFT);

    lv_obj_t *ln = lv_obj_create(lv_scr_act());
    lv_obj_remove_style_all(ln);
    lv_obj_set_pos(ln, 14, 80); lv_obj_set_size(ln, 212, 1);
    lv_obj_set_style_bg_opa(ln, LV_OPA_COVER, 0);
    lv_obj_set_style_bg_color(ln, lv_color_hex(C_LINE), 0);

    s_tv   = label(14, 90, 212, C_TEXT, &font_cn_16, LV_TEXT_ALIGN_LEFT);
    s_tvip = label(14, 110, 212, C_MUTED, &font_cn_16, LV_TEXT_ALIGN_LEFT);

    s_dot = lv_obj_create(lv_scr_act());
    lv_obj_remove_style_all(s_dot);
    lv_obj_set_pos(s_dot, 14, 145); lv_obj_set_size(s_dot, 10, 10);
    lv_obj_set_style_radius(s_dot, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_opa(s_dot, LV_OPA_COVER, 0);
    s_state = label(32, 140, 194, C_TEXT, &font_cn_16, LV_TEXT_ALIGN_LEFT);
    s_sub   = label(14, 162, 212, C_MUTED, &font_cn_16, LV_TEXT_ALIGN_LEFT);

    s_page  = label(14, 176, 100, C_TEXT, &lv_font_montserrat_44, LV_TEXT_ALIGN_LEFT);
    s_pages = label(120, 200, 106, C_MUTED, &font_cn_16, LV_TEXT_ALIGN_RIGHT);
}

static void render(const lcd_status_t *s)
{
    char b[96];
    if (s->ip[0]) snprintf(b, sizeof b, "Wi-Fi  %s", s->ssid);
    else          snprintf(b, sizeof b, "Wi-Fi  %s  连接中…", s->ssid);
    lv_label_set_text(s_wifi, b);
    snprintf(b, sizeof b, "%s", s->ip[0] ? s->ip : "");
    lv_label_set_text(s_ip, b);

    if (s->tv_name[0]) {
        snprintf(b, sizeof b, "电视  %s", s->tv_name);
        lv_label_set_text(s_tv, b);
        lv_label_set_text(s_tvip, s->tv_ip);
    } else {
        lv_label_set_text(s_tv, "电视  —");
        lv_label_set_text(s_tvip, "");
    }

    uint32_t c = C_INFO;
    const char *st = "";
    b[0] = 0;
    switch (s->state) {
    case ST_WIFI:      c = C_INFO; st = "连接 Wi-Fi…"; break;
    case ST_SEARCH:    c = C_INFO; st = "正在搜索电视…"; break;
    case ST_NO_TV:     c = C_BAD;  st = "未找到电视";
        snprintf(b, sizeof b, "%d 秒后重试", s->retry_in_s); break;
    case ST_PUSH_FAIL: c = C_BAD;  st = "推送失败";
        snprintf(b, sizeof b, "%d 秒后重试", s->retry_in_s); break;
    case ST_PUSHED:    c = C_WAIT; st = "已推送，等电视拉流"; break;
    case ST_STREAMING: c = C_OK;   st = "推流中";
        snprintf(b, sizeof b, "已发 %lu 帧", (unsigned long)s->frames); break;
    }
    lv_obj_set_style_bg_color(s_dot, lv_color_hex(c), 0);
    lv_obj_set_style_text_color(s_state, lv_color_hex(c), 0);
    lv_label_set_text(s_state, st);
    lv_label_set_text(s_sub, b);

    snprintf(b, sizeof b, "%d", s->page);
    lv_label_set_text(s_page, b);
    snprintf(b, sizeof b, "第 %d / %d 页", s->page, s->pages);
    lv_label_set_text(s_pages, b);
}

static void lcd_task(void *arg)
{
    (void)arg;
    lcd_status_t s;
    while (1) {
        memset(&s, 0, sizeof s);
        lcd_fill_status(&s);
        if (bsp_display_lock(100)) {
            render(&s);
            bsp_display_unlock();
        }
        vTaskDelay(pdMS_TO_TICKS(500));
    }
}

void lcd_start(void)
{
    if (!display_start()) {
        ESP_LOGE(TAG, "屏幕初始化失败——不影响推流");
        return;
    }
    bsp_display_brightness_set(80);
    bsp_display_lock(0);
    build();
    bsp_display_unlock();
    xTaskCreate(lcd_task, "lcd", 4096, NULL, 2, NULL);
}
