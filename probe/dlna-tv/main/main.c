/*
 * DLNA 电视推流探针：ESP32 独立完成「找电视 → 推送 → 持续发流 → 换内容」，不靠任何别的设备。
 *
 * 为什么是这条路（见 docs/HANDOFF-usb-tv-probe.md 的实验记录）：
 *   - USB 虚拟 U 盘只有软拔插能让电视重读，每次换内容都退回首页，约 15 秒
 *   - DLNA 推图片每次都弹底部工具条
 *   - DLNA 推一路「永不结束的视频」，换内容只是换帧——丝滑、无工具条
 *   - 这台电视 MJPEG 只到 1080p；4K 要 H.264，ESP32-S3 编不动。所以这里是 1080p MJPEG
 *
 * 第一步：帧是 flash 里预置的 1080p JPEG。下一步换成板上渲染。
 */
#include <string.h>

#include "esp_event.h"
#include "esp_log.h"
#include "esp_netif.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"
#include "freertos/task.h"
#include "nvs_flash.h"

#include <time.h>
#include <sys/time.h>

#include "esp_sntp.h"

#include "clockface.h"
#include "ctrl.h"
#include "dlna.h"
#include "lcd.h"
#include "stream.h"

static const char *TAG = "main";

extern const uint8_t f1_start[] asm("_binary_page1_jpg_start");
extern const uint8_t f1_end[] asm("_binary_page1_jpg_end");
extern const uint8_t f2_start[] asm("_binary_page2_jpg_start");
extern const uint8_t f2_end[] asm("_binary_page2_jpg_end");
extern const uint8_t f3_start[] asm("_binary_page3_jpg_start");
extern const uint8_t f3_end[] asm("_binary_page3_jpg_end");

/* 每页的背景色：必须和 tools/render_pages.py 的 PAGES 一致，时钟带用它当底色 */
static const uint32_t PAGE_BG[] = { 0x1E3A8A, 0x7F1D1D, 0x14532D };

static EventGroupHandle_t s_evt;
#define EVT_GOT_IP BIT0
static char s_ip[16];

static dlna_tv_t s_tv;
static volatile bool s_tv_found;
static volatile bool s_repush;
static volatile lcd_state_t s_state = ST_WIFI;
static volatile int64_t s_retry_at_us;

static void on_wifi(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    if (base == WIFI_EVENT && (id == WIFI_EVENT_STA_START || id == WIFI_EVENT_STA_DISCONNECTED)) {
        xEventGroupClearBits(s_evt, EVT_GOT_IP);
        esp_wifi_connect();
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        ip_event_got_ip_t *e = data;
        snprintf(s_ip, sizeof s_ip, IPSTR, IP2STR(&e->ip_info.ip));
        ESP_LOGW(TAG, "★ 控制面板： http://%s/", s_ip);
        xEventGroupSetBits(s_evt, EVT_GOT_IP);
    }
}

static void wifi_start(void)
{
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    esp_netif_create_default_wifi_sta();
    wifi_init_config_t wc = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&wc));
    ESP_ERROR_CHECK(esp_event_handler_register(WIFI_EVENT, ESP_EVENT_ANY_ID, on_wifi, NULL));
    ESP_ERROR_CHECK(esp_event_handler_register(IP_EVENT, IP_EVENT_STA_GOT_IP, on_wifi, NULL));

    wifi_config_t sta = { 0 };
    strlcpy((char *)sta.sta.ssid, CONFIG_PROBE_WIFI_SSID, sizeof sta.sta.ssid);
    strlcpy((char *)sta.sta.password, CONFIG_PROBE_WIFI_PASS, sizeof sta.sta.password);
    sta.sta.scan_method = WIFI_ALL_CHANNEL_SCAN;
    sta.sta.sort_method = WIFI_CONNECT_AP_BY_SIGNAL;
    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
    ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_STA, &sta));
    ESP_ERROR_CHECK(esp_wifi_start());
    esp_wifi_set_ps(WIFI_PS_NONE); /* 持续发流，省电模式会让吞吐忽高忽低 */
}

/*
 * 守护：找电视 → 推送 → 盯着流。流空闲超过 REPUSH_IDLE_S（电视关机 / 切走信号源 / IP 变了）
 * 就重新发现并重推；失败按 2 倍退避到 5 分钟，避免电视开着别的东西时被反复抢屏。
 */
static void supervisor_task(void *arg)
{
    (void)arg;
    char url[64];
    int backoff = CONFIG_DLNA_REPUSH_IDLE_S;
    int64_t pushed_at = 0;

    while (1) {
        xEventGroupWaitBits(s_evt, EVT_GOT_IP, pdFALSE, pdFALSE, portMAX_DELAY);
        int64_t now = esp_timer_get_time();
        int64_t last = stream_last_active_us();
        bool streaming = stream_clients() > 0 && now - last < 5000000;
        bool idle_long = !streaming && (now - (last > pushed_at ? last : pushed_at)) > (int64_t)backoff * 1000000;
        if (streaming) s_state = ST_STREAMING;
        else if (s_state == ST_STREAMING) s_state = ST_PUSHED;  /* 流断了，等空闲超时后重推 */
        if (s_state == ST_WIFI) s_state = ST_SEARCH;

        if (s_repush || pushed_at == 0 || idle_long) {
            bool manual = s_repush;
            s_repush = false;
            s_state = ST_SEARCH;
            s_tv_found = dlna_find_tv(CONFIG_DLNA_TV_IP, &s_tv);
            if (s_tv_found) {
                snprintf(url, sizeof url, "http://%s:%d/live.avi", s_ip, STREAM_PORT);
                if (dlna_push(&s_tv, url, "video/avi", "StickBox")) {
                    backoff = CONFIG_DLNA_REPUSH_IDLE_S;
                    s_state = ST_PUSHED;
                } else {
                    backoff = backoff * 2 > 300 ? 300 : backoff * 2;
                    s_state = ST_PUSH_FAIL;
                }
            } else {
                backoff = backoff * 2 > 300 ? 300 : backoff * 2;
                s_state = ST_NO_TV;
            }
            pushed_at = esp_timer_get_time();
            s_retry_at_us = pushed_at + (int64_t)backoff * 1000000;
            if (!manual) ESP_LOGI(TAG, "下次检查在 %d 秒后", backoff);
        }
        vTaskDelay(pdMS_TO_TICKS(1000));
    }
}

/*
 * 时钟：每 100ms 看一次，秒变了（或换页了）就重新编码时钟带并发布。
 * 电视播放器会先缓冲几秒再播，所以编码的是「现在 + CLOCK_LEAD」——
 * 等这一帧真正出现在电视上时，时间刚好对。
 */
static void clock_task(void *arg)
{
    (void)arg;
    clockface_t cf;
    char last[9] = "";
    bool last_colon = false;
    int last_page = -1;
    while (1) {
        struct timeval tv;
        gettimeofday(&tv, NULL);
        int64_t ms = (int64_t)tv.tv_sec * 1000 + tv.tv_usec / 1000 + CONFIG_DLNA_CLOCK_LEAD_MS;
        time_t t = ms / 1000;
        struct tm tm;
        localtime_r(&t, &tm);
        if (tm.tm_year < 2025 - 1900) strcpy(cf.txt, "--:--");  /* 还没对上时 */
        else snprintf(cf.txt, sizeof cf.txt, "%02d:%02d", tm.tm_hour, tm.tm_min);
        cf.colon_on = !(tm.tm_sec & 1);

        int page = stream_current();
        if (strcmp(cf.txt, last) || cf.colon_on != last_colon || page != last_page) {
            int64_t t0 = esp_timer_get_time();
            uint8_t *buf = stream_band_begin();
            size_t n = jb_encode_rows(stream_page(page), CLOCK_ROW0, CLOCK_ROW0 + CLOCK_ROWS,
                                      PAGE_BG[page], 0xFFFFFF, clockface_shade, &cf, &CLOCKFACE_BOX, buf, BAND_CAP);
            if (n) stream_band_publish(buf, n, page);
            else ESP_LOGE(TAG, "时钟带超出缓冲 %d B", BAND_CAP);
            int64_t dt = (esp_timer_get_time() - t0) / 1000;
            if (page != last_page || strcmp(cf.txt, last))
                ESP_LOGI(TAG, "时钟带 %s：%u B，编码 %lld ms", cf.txt, (unsigned)n, (long long)dt);
            strcpy(last, cf.txt);
            last_colon = cf.colon_on;
            last_page = page;
        }
        vTaskDelay(pdMS_TO_TICKS(100));
    }
}

static void sntp_start(void)
{
    setenv("TZ", "CST-8", 1);
    tzset();
    esp_sntp_setoperatingmode(SNTP_OPMODE_POLL);
    esp_sntp_setservername(0, "ntp.aliyun.com");
    esp_sntp_setservername(1, "pool.ntp.org");
    esp_sntp_init();
}

void lcd_fill_status(lcd_status_t *o)
{
    o->state = s_state;
    strlcpy(o->ssid, CONFIG_PROBE_WIFI_SSID, sizeof o->ssid);
    if (xEventGroupGetBits(s_evt) & EVT_GOT_IP) strlcpy(o->ip, s_ip, sizeof o->ip);
    else o->state = ST_WIFI;
    if (s_tv_found) {
        strlcpy(o->tv_name, s_tv.name, sizeof o->tv_name);
        strlcpy(o->tv_ip, s_tv.ip, sizeof o->tv_ip);
    }
    o->page = stream_current() + 1;
    o->pages = stream_frame_count();
    o->frames = stream_frames_sent();
    int64_t left = (s_retry_at_us - esp_timer_get_time()) / 1000000;
    o->retry_in_s = left > 0 ? (int)left : 0;
}

static void cycle_task(void *arg)
{
    (void)arg;
    while (1) {
        vTaskDelay(pdMS_TO_TICKS(CONFIG_DLNA_CYCLE_S * 1000));
        stream_set(stream_current() + 1);
        ESP_LOGI(TAG, "换到第 %d 页", stream_current() + 1);
    }
}

void app_main(void)
{
    ESP_LOGI(TAG, "=== DLNA 电视推流探针 ===");
    esp_err_t r = nvs_flash_init();
    if (r == ESP_ERR_NVS_NO_FREE_PAGES || r == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_ERROR_CHECK(nvs_flash_erase());
        ESP_ERROR_CHECK(nvs_flash_init());
    }
    s_evt = xEventGroupCreate();

    static frame_t frames[3];
    frames[0] = (frame_t){ f1_start, f1_end - f1_start };
    frames[1] = (frame_t){ f2_start, f2_end - f2_start };
    frames[2] = (frame_t){ f3_start, f3_end - f3_start };

    /* 顺序要紧：wifi_start() 里的 esp_netif_init() 起 lwip；在它之前建 socket 会
     * assert「tcpip_send_msg_wait_sem: Invalid mbox」然后无限重启（实测） */
    wifi_start();
    stream_start(frames, 3, CONFIG_DLNA_FPS, CLOCK_ROW0, CLOCK_ROWS);
    sntp_start();
    xTaskCreate(clock_task, "clock", 6144, NULL, 4, NULL);
    lcd_start();
    ctrl_start(&s_tv, &s_tv_found, &s_repush);
    xTaskCreate(supervisor_task, "dlna_sup", 6144, NULL, 4, NULL);
    if (CONFIG_DLNA_CYCLE_S > 0) xTaskCreate(cycle_task, "cycle", 2048, NULL, 3, NULL);
}
