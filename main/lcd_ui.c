/* 板载 ST7789：Claude 订阅用量屏（LVGL），按设计稿 1b「270° 仪表环」实现
 * 设计稿：~/Desktop/StickBox 用量屏.html；状态全集见稿中 1g。
 *
 * 数据来源：电脑上的 launchd 定时任务跑 `claude -p /usage`，把用量推到
 * POST /api/usage（见 usage_http.c、tools/claude-usage-push.sh）。
 *
 * ── 版面（240x240）──
 * 坐标以设计稿为准，但实机底部约 12px 被屏幕边框挡住（2026-10-17 实拍），
 * 所以整体比设计稿上移：环上移 4px（顶端刻度离屏幕上沿只剩 1px，不能再多），
 * 环下面的内容再压紧，页脚从 y218 提到 y204。下图标注的是实际坐标。
 *        ╭───────────╮          270° 环：圆心 (120,92)，半径 78，线宽 14
 *      ╱     7 天      ╲         y 46  窗口名
 *     │      34%        │        y 68  大百分比（Montserrat 44 + 22）
 *     │   剩4天18时     │        y 120 剩余时间
 *      ╲               ╱         白色径向刻度 = 窗口时间已过去的比例
 *         约4天7时用完            y 152 预测
 *   ─────────────────────────    y 176 分隔线
 *   5 小时 2%     [▬▬│▬▬▬▬▬]     y 182 次要窗口：名字 + 百分比 + 小进度条
 *   ■□□□            更新 3分前    y 204 页码点 + 数据新鲜度
 *
 * ── 自动主次 ──
 * 更紧迫的窗口当主角上环，另一个缩成一行。先比状态
 * （用满 > 危险 > 超速 > 安全 > 已重置/无数据），同级再比「用量 ÷ 时间」。
 *
 * ── 颜色即告警 ──
 * 平稳时全屏只有钢蓝；琥珀 = 按当前速度会在重置前用完；红 = 已用 90% 以上。
 * 读法：环/条的填充在白刻度之前 = 用得比时间慢；越过白刻度 = 超前消耗。
 *
 * ── 时间 ──
 * 板子没有 SNTP。每次上报带发送方的 Unix 时间 now，这里记下它和开机时长的差，
 * 之后倒计时全靠本地时钟推算，两次上报之间也照样走。
 *
 * ── 屏幕倒装 ──
 * USB 口只能倒插，板子装上后屏幕是倒的。CONFIG_STICKBOX_LCD_ROTATE_180 打开时
 * 用 ST7789 的 MADCTL 镜像 X/Y 翻转，纯硬件、不占渲染时间。
 * 坑：ST7789 显存是 240x320，这块屏只接了前 240 行。行方向一镜像，可见区就落到
 * 显存的 80..319 行，所以必须补一个 y=80 的 gap，否则画面错位、底部出一条花屏。
 *
 * ── 防残影 ──
 * 空闲降亮度 + 每隔几分钟整屏挪几个像素。只改显示，不影响业务。
 */
#include <math.h>
#include <stdio.h>
#include <string.h>
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "sdkconfig.h"
#include "bsp/esp-bsp.h"
#include "esp_lcd_panel_ops.h"
#include "esp_lvgl_port.h"
#include "esp_timer.h"
#include "esp_log.h"
#include "lvgl.h"
#include "lcd_ui.h"

LV_FONT_DECLARE(font_cn_16);

static const char *TAG = "lcd_ui";

/* ── 防残影参数 ── */
#define SHIFT_PERIOD_S   120     /* 多久挪一次位置 */
#define SHIFT_STEP_PX      3     /* 每次挪几个像素 */
#define SHIFT_STATES       8     /* 偏移循环几个位置（走一个方框） */
#define IDLE_DIM_S       300     /* 多久没有新数据就降亮度 */
#define BRIGHT_ACTIVE    100     /* 活动时亮度 % */
#define BRIGHT_IDLE       12     /* 空闲时亮度 %。设 0 就是全黑息屏 */

/* ── 判定阈值 ── */
#define PCT_DANGER        90     /* 用到这么多直接红 */
#define PROJECT_MIN_FRAC  0.05f  /* 窗口过去不到 5% 时速度不准，不做预测 */
#define STALE_S          900     /* 超过 15 分钟没新数据，「更新」变琥珀 */

/* ── 版面（与设计稿 1b 一一对应）── */
#define TEXT_W      234          /* 居中文字的宽度：右边留 6px 给像素偏移 */
#define RING_CX     120
#define RING_CY      92
#define RING_R       78          /* 环中心线半径 */
#define RING_W       14
#define RING_A0     135          /* 起始角（0° = 3 点钟，顺时针），开口朝下 */
#define RING_SWEEP  270
#define Y_LABEL      46
#define Y_NUM        68
#define Y_REM       120
#define Y_PRED      152
#define Y_RULE      176
#define Y_SEC       182
#define SEC_BAR_X   118
#define SEC_BAR_Y   189
#define SEC_BAR_W   110
#define SEC_BAR_H     6
#define Y_FOOT      204
#define PAGE_COUNT    4          /* 设计稿的翻页点；目前只实现了主页 */

/* ── 配色（设计稿 Industry 钢蓝体系）── */
#define C_BG        0x000000
#define C_SAFE      0xb5d9fd
#define C_WARN      0xffb020
#define C_BAD       0xff5a5a
#define C_OFF       0x5d5d60
#define C_MUTED     0x98989b
#define C_LABEL     0x749dc4
#define C_TRACK     0x1d2d3d
#define C_SCALE     0x416180
#define C_RULE      0x2c455d
#define C_TICK      0xf5f5f8
#define C_DOT_ON    0x94bce3

/* 窗口的状态等级：数值越大越紧迫，主次就按它排 */
enum { RANK_NONE, RANK_SAFE, RANK_WARN, RANK_DANGER, RANK_FULL };

typedef struct {
    const char    *name;
    int64_t        len_s;        /* 窗口总长：算时间进度用 */
    usage_window_t w;            /* 最近一次收到的数据，缺席时保留旧值 */
} window_t;

static window_t s_win[2] = {
    { .name = "5 小时", .len_s = 5 * 3600 },
    { .name = "7 天",   .len_s = 7 * 86400 },
};

/* 某一时刻一个窗口的判定结果 */
typedef struct {
    int      rank;
    float    u, t;               /* 用量、时间进度，0..1 */
    int64_t  remain;
    uint32_t color;
    char     pred[32];
} wstat_t;

static lv_obj_t *s_root;         /* 所有内容的容器，像素偏移就是挪它 */
/* 主窗口 */
static lv_obj_t *s_arc, *s_time_tick, *s_lbl_name, *s_lbl_num, *s_lbl_unit, *s_lbl_rem, *s_lbl_pred;
static lv_obj_t *s_num_row;
static lv_point_precise_t s_time_tick_pts[2];
/* 次要窗口 */
static lv_obj_t *s_sec_name, *s_sec_num, *s_sec_fill, *s_sec_tick;
/* 页脚 */
static lv_obj_t *s_lbl_foot, *s_lbl_ip;

static bool    s_have_data;
static int64_t s_epoch_off;      /* Unix 秒 - 开机秒 */
static int64_t s_rx_us;          /* 最近一次收到数据的开机时刻 */
static char    s_ip[20] = "--";

/* 屏幕更新一律走队列，由专门的任务渲染。
 * 原因：LVGL 渲染中文很吃栈，之前直接在网络任务里调，栈溢出把整机干重启过。 */
enum { UI_IP, UI_USAGE };
typedef struct {
    uint8_t kind;
    union { char txt[20]; usage_report_t u; };
} ui_msg_t;
static QueueHandle_t s_q;

static int64_t s_last_activity_us;
static bool    s_dimmed;

/* ── 显示初始化 ──
 * 不用 bsp_display_start_with_config()：它不交出 panel 句柄，没法设镜像和 gap。
 * 这里把它拆开自己做一遍，其余（SPI、背光 PWM、ST7789 初始化）仍复用 BSP。 */
static bool display_start(void)
{
    const lvgl_port_cfg_t pcfg = ESP_LVGL_PORT_INIT_CONFIG();
    if (lvgl_port_init(&pcfg) != ESP_OK) {
        ESP_LOGE(TAG, "lvgl_port_init failed");
        return false;
    }

    esp_lcd_panel_handle_t panel = NULL;
    esp_lcd_panel_io_handle_t io = NULL;
    const bsp_display_config_t bcfg = {
        .max_transfer_sz = BSP_LCD_DRAW_BUFF_SIZE * sizeof(uint16_t),
    };
    if (bsp_display_new(&bcfg, &panel, &io) != ESP_OK) {
        ESP_LOGE(TAG, "bsp_display_new failed");
        return false;
    }

#if CONFIG_STICKBOX_LCD_ROTATE_180
    const bool flip = true;
    esp_lcd_panel_mirror(panel, true, true);
    esp_lcd_panel_set_gap(panel, 0, 320 - BSP_LCD_V_RES);   /* 见文件头「坑」 */
#else
    const bool flip = false;
#endif
    esp_lcd_panel_disp_on_off(panel, true);

    const lvgl_port_display_cfg_t dcfg = {
        .io_handle     = io,
        .panel_handle  = panel,
        .buffer_size   = BSP_LCD_H_RES * 16,   /* 双缓冲 240x30 太奢侈，状态屏够用即可 */
        .double_buffer = false,
        .hres          = BSP_LCD_H_RES,
        .vres          = BSP_LCD_V_RES,
        .monochrome    = false,
        /* 必须和上面实际写进面板的镜像一致：lvgl_port 以后处理旋转时以此为基准 */
        .rotation      = { .swap_xy = false, .mirror_x = flip, .mirror_y = flip },
        .flags = {
            .buff_dma    = true,
            .buff_spiram = false,
            .swap_bytes  = BSP_LCD_BIGENDIAN ? true : false,
        },
    };
    if (!lvgl_port_add_disp(&dcfg)) {
        ESP_LOGE(TAG, "lvgl_port_add_disp failed");
        return false;
    }
    return true;
}

/* ── 控件工厂 ── */
static lv_obj_t *mk_label(lv_obj_t *parent, int x, int y, int w, uint32_t color,
                          const lv_font_t *font, lv_text_align_t align)
{
    lv_obj_t *l = lv_label_create(parent);
    lv_obj_set_pos(l, x, y);
    if (w > 0) lv_obj_set_width(l, w);
    lv_label_set_long_mode(l, LV_LABEL_LONG_MODE_CLIP);
    lv_obj_set_style_text_color(l, lv_color_hex(color), 0);
    lv_obj_set_style_text_font(l, font, 0);
    lv_obj_set_style_text_align(l, align, 0);
    lv_label_set_text(l, "");
    return l;
}

static lv_obj_t *mk_rect(int x, int y, int w, int h, uint32_t color)
{
    lv_obj_t *r = lv_obj_create(s_root);
    lv_obj_remove_style_all(r);
    lv_obj_set_pos(r, x, y);
    lv_obj_set_size(r, w, h);
    lv_obj_set_style_bg_opa(r, LV_OPA_COVER, 0);
    lv_obj_set_style_bg_color(r, lv_color_hex(color), 0);
    return r;
}

/* 环上角度 a（度）、半径 rr 处的点 */
static lv_point_precise_t ring_pt(float a, float rr)
{
    float q = a * (float)M_PI / 180.0f;
    return (lv_point_precise_t){ (int32_t)lroundf(RING_CX + rr * cosf(q)),
                                 (int32_t)lroundf(RING_CY + rr * sinf(q)) };
}

static lv_obj_t *mk_line(lv_point_precise_t *pts, int width, uint32_t color)
{
    lv_obj_t *l = lv_line_create(s_root);
    lv_obj_set_pos(l, 0, 0);
    lv_obj_set_style_line_width(l, width, 0);
    lv_obj_set_style_line_color(l, lv_color_hex(color), 0);
    lv_line_set_points(l, pts, 2);       /* LVGL 只存指针：pts 必须是静态存储 */
    return l;
}

static void build_ring(void)
{
    /* lv_arc 以控件外沿为半径往里画：外半径 = 中心线半径 + 半线宽 */
    const int outer = RING_R + RING_W / 2;
    s_arc = lv_arc_create(s_root);
    lv_obj_remove_style_all(s_arc);
    lv_obj_set_pos(s_arc, RING_CX - outer, RING_CY - outer);
    lv_obj_set_size(s_arc, 2 * outer, 2 * outer);
    lv_arc_set_rotation(s_arc, RING_A0);
    lv_arc_set_bg_angles(s_arc, 0, RING_SWEEP);
    lv_arc_set_range(s_arc, 0, 1000);
    lv_arc_set_value(s_arc, 0);
    lv_obj_remove_flag(s_arc, LV_OBJ_FLAG_CLICKABLE);
    for (int part = 0; part < 2; part++) {
        lv_style_selector_t sel = part ? LV_PART_INDICATOR : LV_PART_MAIN;
        lv_obj_set_style_arc_width(s_arc, RING_W, sel);
        lv_obj_set_style_arc_rounded(s_arc, false, sel);   /* 设计稿是方头 */
        lv_obj_set_style_arc_opa(s_arc, LV_OPA_COVER, sel);
    }
    lv_obj_set_style_arc_color(s_arc, lv_color_hex(C_TRACK), LV_PART_MAIN);
    lv_obj_set_style_arc_color(s_arc, lv_color_hex(C_SAFE), LV_PART_INDICATOR);

    /* 外圈 5 个刻度：0 / 25 / 50 / 75 / 100% */
    static lv_point_precise_t scale_pts[5][2];
    for (int i = 0; i < 5; i++) {
        float a = RING_A0 + i * 0.25f * RING_SWEEP;
        scale_pts[i][0] = ring_pt(a, RING_R + 9);
        scale_pts[i][1] = ring_pt(a, RING_R + 13);
        mk_line(scale_pts[i], 1, C_SCALE);
    }

    /* 时间刻度：横跨环的白色径向短线，位置每秒更新 */
    s_time_tick = mk_line(s_time_tick_pts, 2, C_TICK);
    lv_obj_add_flag(s_time_tick, LV_OBJ_FLAG_HIDDEN);
}

static void build_main_text(void)
{
    s_lbl_name = mk_label(s_root, 0, Y_LABEL, TEXT_W, C_LABEL, &font_cn_16, LV_TEXT_ALIGN_CENTER);

    /* 大数字 + 小「%」底部对齐、整体居中：用一行 flex 容器 */
    s_num_row = lv_obj_create(s_root);
    lv_obj_remove_style_all(s_num_row);
    lv_obj_set_pos(s_num_row, 0, Y_NUM);
    lv_obj_set_size(s_num_row, TEXT_W, LV_SIZE_CONTENT);
    lv_obj_set_flex_flow(s_num_row, LV_FLEX_FLOW_ROW);
    lv_obj_set_flex_align(s_num_row, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_END, LV_FLEX_ALIGN_END);
    lv_obj_set_style_pad_column(s_num_row, 2, 0);
    s_lbl_num  = mk_label(s_num_row, 0, 0, 0, C_SAFE, &lv_font_montserrat_44, LV_TEXT_ALIGN_LEFT);
    s_lbl_unit = mk_label(s_num_row, 0, 0, 0, C_SAFE, &lv_font_montserrat_22, LV_TEXT_ALIGN_LEFT);
    /* flex 只能底边对齐，两种字号的基线差要手动补上 */
    lv_obj_set_style_pad_bottom(s_lbl_unit,
        lv_font_montserrat_44.base_line - lv_font_montserrat_22.base_line, 0);

    s_lbl_rem  = mk_label(s_root, 0, Y_REM,  TEXT_W, C_MUTED, &font_cn_16, LV_TEXT_ALIGN_CENTER);
    s_lbl_pred = mk_label(s_root, 0, Y_PRED, TEXT_W, C_SAFE,  &font_cn_16, LV_TEXT_ALIGN_CENTER);
}

static void build_secondary(void)
{
    mk_rect(6, Y_RULE, 222, 1, C_RULE);

    lv_obj_t *row = lv_obj_create(s_root);
    lv_obj_remove_style_all(row);
    lv_obj_set_pos(row, 6, Y_SEC);
    lv_obj_set_size(row, LV_SIZE_CONTENT, 20);
    lv_obj_set_flex_flow(row, LV_FLEX_FLOW_ROW);
    lv_obj_set_flex_align(row, LV_FLEX_ALIGN_START, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_column(row, 6, 0);
    s_sec_name = mk_label(row, 0, 0, 0, C_LABEL, &font_cn_16, LV_TEXT_ALIGN_LEFT);
    s_sec_num  = mk_label(row, 0, 0, 0, C_SAFE, &lv_font_montserrat_20, LV_TEXT_ALIGN_LEFT);

    mk_rect(SEC_BAR_X, SEC_BAR_Y, SEC_BAR_W, SEC_BAR_H, C_TRACK);
    s_sec_fill = mk_rect(SEC_BAR_X, SEC_BAR_Y, 0, SEC_BAR_H, C_SAFE);
    s_sec_tick = mk_rect(SEC_BAR_X, SEC_BAR_Y - 3, 2, 12, C_TICK);
    lv_obj_add_flag(s_sec_tick, LV_OBJ_FLAG_HIDDEN);
}

static void build_footer(void)
{
    /* 页码点：第一个实心，其余描边 */
    for (int i = 0; i < PAGE_COUNT; i++) {
        lv_obj_t *d = mk_rect(6 + i * 10, Y_FOOT + 5, 6, 6, C_DOT_ON);
        if (i) {
            lv_obj_set_style_bg_opa(d, LV_OPA_TRANSP, 0);
            lv_obj_set_style_border_width(d, 1, 0);
            lv_obj_set_style_border_color(d, lv_color_hex(C_SCALE), 0);
        }
    }
    s_lbl_foot = mk_label(s_root, 6, Y_FOOT, 222, C_MUTED, &font_cn_16, LV_TEXT_ALIGN_RIGHT);

    /* 无数据时盖在页码点上显示 IP（黑底挡住点） */
    s_lbl_ip = mk_label(s_root, 6, Y_FOOT, 0, C_MUTED, &lv_font_montserrat_14, LV_TEXT_ALIGN_LEFT);
    lv_obj_set_style_bg_opa(s_lbl_ip, LV_OPA_COVER, 0);
    lv_obj_set_style_bg_color(s_lbl_ip, lv_color_hex(C_BG), 0);
    lv_obj_set_style_pad_ver(s_lbl_ip, 1, 0);
}

/* 秒 → 「4天18时」「2时24分」「42分」 */
static void fmt_dur(char *buf, size_t n, int64_t s)
{
    if (s < 0) s = 0;
    if (s >= 86400)     snprintf(buf, n, "%d天%d时", (int)(s / 86400), (int)(s % 86400 / 3600));
    else if (s >= 3600) snprintf(buf, n, "%d时%02d分", (int)(s / 3600), (int)(s % 3600 / 60));
    else                snprintf(buf, n, "%d分", (int)((s + 59) / 60));
}

static int64_t uptime_s(void) { return esp_timer_get_time() / 1000000; }

/* 判定一个窗口此刻的状态 */
static void judge(const window_t *win, int64_t now, wstat_t *st)
{
    memset(st, 0, sizeof *st);
    st->color = C_OFF;
    if (!win->w.present) return;                          /* 从没收到过 */
    st->remain = win->w.resets_at - now;
    if (st->remain <= 0) {                                /* 过了重置点，旧数作废 */
        snprintf(st->pred, sizeof st->pred, "等新数据");
        return;
    }

    float pct = win->w.used_pct;
    if (pct < 0) pct = 0;
    if (pct > 100) pct = 100;
    int64_t remain = st->remain > win->len_s ? win->len_s : st->remain;
    int64_t elapsed = win->len_s - remain;
    st->u = pct / 100.0f;
    st->t = (float)elapsed / (float)win->len_s;

    /* 预测：按窗口开始到现在的平均速度，什么时候用到 100% */
    st->rank = RANK_SAFE;
    st->color = C_SAFE;
    snprintf(st->pred, sizeof st->pred, "够用到重置");
    if (pct >= 100) {
        st->rank = RANK_FULL;
        st->color = C_BAD;
        snprintf(st->pred, sizeof st->pred, "已用满");
        return;
    }
    if (st->t >= PROJECT_MIN_FRAC && pct > 0) {
        float t_full = (100.0f - pct) * (float)elapsed / pct;   /* 还能撑多少秒 */
        if (t_full < (float)remain) {
            char d[20];
            fmt_dur(d, sizeof d, (int64_t)t_full);
            snprintf(st->pred, sizeof st->pred, "约%s用完", d);
            st->rank = RANK_WARN;
            st->color = C_WARN;
        }
    }
    if (pct >= PCT_DANGER) {
        st->rank = RANK_DANGER;
        st->color = C_BAD;
    }
}

/* 主次：先比等级，同级比「用量 ÷ 时间」；都没数据时 5 小时当主角 */
static int pick_main(const wstat_t st[2])
{
    if (st[0].rank != st[1].rank) return st[0].rank > st[1].rank ? 0 : 1;
    if (st[0].rank == RANK_NONE) return 0;
    float r0 = st[0].u / (st[0].t > 0.01f ? st[0].t : 0.01f);
    float r1 = st[1].u / (st[1].t > 0.01f ? st[1].t : 0.01f);
    return r1 > r0 ? 1 : 0;
}

/* 调用方须已持有 bsp_display_lock */
static void render_main(const window_t *win, const wstat_t *st)
{
    char buf[40], d[20];
    lv_color_t c = lv_color_hex(st->color);

    lv_label_set_text(s_lbl_name, win->name);
    lv_obj_set_style_text_color(s_lbl_num, c, 0);
    lv_obj_set_style_text_color(s_lbl_unit, c, 0);
    lv_obj_set_style_text_color(s_lbl_pred, c, 0);
    lv_obj_set_style_arc_color(s_arc, c, LV_PART_INDICATOR);

    if (st->rank == RANK_NONE) {
        lv_label_set_text(s_lbl_num, "--");
        lv_label_set_text(s_lbl_unit, "");
        lv_label_set_text(s_lbl_rem, "");
        lv_label_set_text(s_lbl_pred, s_have_data ? st->pred : "等待数据");
        lv_arc_set_value(s_arc, 0);
        lv_obj_add_flag(s_time_tick, LV_OBJ_FLAG_HIDDEN);
        return;
    }

    snprintf(buf, sizeof buf, "%d", (int)lroundf(st->u * 100));
    lv_label_set_text(s_lbl_num, buf);
    lv_label_set_text(s_lbl_unit, "%");
    fmt_dur(d, sizeof d, st->remain);
    snprintf(buf, sizeof buf, "剩%s", d);
    lv_label_set_text(s_lbl_rem, buf);
    lv_label_set_text(s_lbl_pred, st->pred);

    lv_arc_set_value(s_arc, (int32_t)lroundf(st->u * 1000));
    float a = RING_A0 + st->t * RING_SWEEP;
    s_time_tick_pts[0] = ring_pt(a, RING_R - 12);
    s_time_tick_pts[1] = ring_pt(a, RING_R + 12);
    lv_line_set_points(s_time_tick, s_time_tick_pts, 2);
    lv_obj_remove_flag(s_time_tick, LV_OBJ_FLAG_HIDDEN);
}

static void render_secondary(const window_t *win, const wstat_t *st)
{
    char buf[16];
    lv_color_t c = lv_color_hex(st->color);
    lv_label_set_text(s_sec_name, win->name);
    lv_obj_set_style_text_color(s_sec_num, c, 0);
    lv_obj_set_style_bg_color(s_sec_fill, c, 0);

    if (st->rank == RANK_NONE) {
        lv_label_set_text(s_sec_num, "--");
        lv_obj_set_width(s_sec_fill, 0);
        lv_obj_add_flag(s_sec_tick, LV_OBJ_FLAG_HIDDEN);
        return;
    }
    snprintf(buf, sizeof buf, "%d%%", (int)lroundf(st->u * 100));
    lv_label_set_text(s_sec_num, buf);
    lv_obj_set_width(s_sec_fill, (int32_t)lroundf(st->u * SEC_BAR_W));
    lv_obj_set_x(s_sec_tick, SEC_BAR_X + (int32_t)lroundf(st->t * SEC_BAR_W) - 1);
    lv_obj_remove_flag(s_sec_tick, LV_OBJ_FLAG_HIDDEN);
}

static void render_footer(void)
{
    char buf[32];
    if (!s_have_data) {
        lv_label_set_text(s_lbl_foot, "POST /api/usage");
        lv_obj_set_style_text_color(s_lbl_foot, lv_color_hex(C_MUTED), 0);
        lv_label_set_text(s_lbl_ip, s_ip);
        lv_obj_remove_flag(s_lbl_ip, LV_OBJ_FLAG_HIDDEN);
        return;
    }
    lv_obj_add_flag(s_lbl_ip, LV_OBJ_FLAG_HIDDEN);
    int age = (int)((esp_timer_get_time() - s_rx_us) / 1000000);
    if (age < 60)        snprintf(buf, sizeof buf, "更新 %d秒前", age);
    else if (age < 3600) snprintf(buf, sizeof buf, "更新 %d分前", age / 60);
    else                 snprintf(buf, sizeof buf, "更新 %d时前", age / 3600);
    lv_label_set_text(s_lbl_foot, buf);
    lv_obj_set_style_text_color(s_lbl_foot, lv_color_hex(age > STALE_S ? C_WARN : C_MUTED), 0);
}

/* 每秒刷一遍：倒计时、时间刻度、预测、主次都跟着时钟走 */
static void render(void)
{
    int64_t now = uptime_s() + s_epoch_off;
    wstat_t st[2];
    for (int i = 0; i < 2; i++) judge(&s_win[i], now, &st[i]);
    int m = pick_main(st);

    bsp_display_lock(0);
    render_main(&s_win[m], &st[m]);
    render_secondary(&s_win[!m], &st[!m]);
    render_footer();
    bsp_display_unlock();
}

static void take_report(const usage_report_t *u)
{
    s_have_data = true;
    s_epoch_off = u->now - uptime_s();
    s_rx_us     = esp_timer_get_time();
    if (u->five_hour.present) s_win[0].w = u->five_hour;
    if (u->seven_day.present) s_win[1].w = u->seven_day;
    render();
}

/* 整屏像素偏移：沿一个小方框循环走位，任何一个像素都不会长期显示同一内容。 */
static void apply_shift(void)
{
    static int state;
    static const int8_t DX[SHIFT_STATES] = { 0, 1, 2, 2, 2, 1, 0, 0 };
    /* 纵向只走一步（3px）：底部被边框挡住，往下挪多了页脚就看不见 */
    static const int8_t DY[SHIFT_STATES] = { 0, 0, 0, 1, 1, 1, 1, 0 };
    state = (state + 1) % SHIFT_STATES;
    bsp_display_lock(0);
    lv_obj_set_pos(s_root, DX[state] * SHIFT_STEP_PX, DY[state] * SHIFT_STEP_PX);
    bsp_display_unlock();
}

static void set_brightness(bool active)
{
    if (s_dimmed == !active) return;          /* 状态没变就别折腾背光 */
    s_dimmed = !active;
    bsp_display_brightness_set(active ? BRIGHT_ACTIVE : BRIGHT_IDLE);
}

static void ui_task(void *a)
{
    ui_msg_t m;
    int64_t last_shift_us = esp_timer_get_time();
    render();
    while (1) {
        /* 1 秒超时兼做时钟：没消息时刷新倒计时和跑防残影 */
        if (xQueueReceive(s_q, &m, pdMS_TO_TICKS(1000)) == pdTRUE) {
            if (m.kind == UI_USAGE) {
                s_last_activity_us = esp_timer_get_time();
                set_brightness(true);         /* 有新数据就点亮 */
                take_report(&m.u);
            } else if (m.kind == UI_IP) {
                strlcpy(s_ip, m.txt, sizeof s_ip);
                render();
            }
            continue;
        }

        render();

        int64_t now = esp_timer_get_time();
        if (now - last_shift_us >= (int64_t)SHIFT_PERIOD_S * 1000000) {
            last_shift_us = now;
            apply_shift();
        }
        if (now - s_last_activity_us >= (int64_t)IDLE_DIM_S * 1000000)
            set_brightness(false);
    }
}

void lcd_ui_init(void)
{
    if (!display_start()) return;             /* 屏起不来不影响打印业务，post 会静默丢弃 */
    bsp_display_brightness_set(BRIGHT_ACTIVE);

    bsp_display_lock(0);
    lv_obj_set_style_bg_color(lv_scr_act(), lv_color_hex(C_BG), 0);

    /* 所有内容挂在这个透明容器里，偏移时整体挪它一个对象就够了 */
    s_root = lv_obj_create(lv_scr_act());
    lv_obj_remove_style_all(s_root);
    lv_obj_set_pos(s_root, 0, 0);
    lv_obj_set_size(s_root, BSP_LCD_H_RES, BSP_LCD_V_RES);
    lv_obj_clear_flag(s_root, LV_OBJ_FLAG_SCROLLABLE);

    build_ring();
    build_main_text();
    build_secondary();
    build_footer();
    bsp_display_unlock();

    s_last_activity_us = esp_timer_get_time();
    s_q = xQueueCreate(4, sizeof(ui_msg_t));
    xTaskCreate(ui_task, "lcd_ui", 6144, NULL, 2, NULL);   /* 渲染独占大栈 */
}

void lcd_ui_ip(const char *t)
{
    if (!s_q) return;
    ui_msg_t m = { .kind = UI_IP };
    strlcpy(m.txt, t, sizeof m.txt);
    xQueueSend(s_q, &m, 0);          /* 满了就丢，绝不阻塞调用方 */
}

void lcd_ui_usage(const usage_report_t *u)
{
    if (!s_q) return;
    ui_msg_t m = { .kind = UI_USAGE, .u = *u };
    xQueueSend(s_q, &m, 0);
}

/* 屏幕只显示用量，打印业务的状态不再上屏；接口留着免得改调用方 */
void lcd_ui_wifi(const char *t)     { (void)t; }
void lcd_ui_prn_name(const char *t) { (void)t; }
void lcd_ui_prn(const char *t)      { (void)t; }
void lcd_ui_job(const char *t)      { (void)t; }
void lcd_ui_log(const char *line)   { (void)line; }
void lcd_ui_beat(void)              { }
