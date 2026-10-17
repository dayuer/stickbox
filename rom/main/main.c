/*
 * StickBox 电视看板固件（T3）：
 *   Wi-Fi station（热点配网） + MQTT 长连接（心跳 / 用量） +
 *   DLNA 推一路永不结束的 MJPEG 流到电视 + 板载 LCD 状态屏。
 *
 * 打印链路已整条退役：USB host、打印机枚举、作业取件、怪癖档案、
 * 适配测试、局域网用量上报都删掉了。电视画面先用 probe 里的预置页，
 * 看板渲染是 T4。
 */
#include <string.h>
#include <time.h>
#include <sys/time.h>

#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"
#include "freertos/task.h"

#include "driver/gpio.h"
#include "esp_event.h"
#include "esp_log.h"
#include "esp_netif.h"
#include "esp_sntp.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "nvs_flash.h"

#include "cloud_client.h"
#include "coredump.h"
#include "dlna.h"
#include "lcd_ui.h"
#include "memdiag.h"
#include "ota.h"
#include "pairband.h"
#include "provision.h"
#include "stream.h"

static const char *TAG = "stickbox";

extern const uint8_t blank_start[] asm("_binary_blank_jpg_start");
extern const uint8_t blank_end[]   asm("_binary_blank_jpg_end");

static EventGroupHandle_t s_evt;
#define EVT_GOT_IP BIT0

static char s_ip[16];
static char s_ssid[33], s_pass[65];
static dlna_tv_t s_tv;
static volatile bool s_tv_found;

static void on_wifi(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    (void)arg;
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        esp_wifi_connect();
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        wifi_event_sta_disconnected_t *d = data;
        ESP_LOGW(TAG, "Wi-Fi 断开 reason=%d rssi=%d，重连", d->reason, d->rssi);
        xEventGroupClearBits(s_evt, EVT_GOT_IP);
        lcd_ui_wifi("Wi-Fi 重连中");
        /* 从未连上过且连错 12 次：多半是换了路由器或改了密码，回配网模式 */
        static int fails;
        if (!s_ip[0] && ++fails >= 12) {
            ESP_LOGE(TAG, "始终连不上，清配置重启进配网");
            prov_erase();
            esp_restart();
        }
        esp_wifi_connect();
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        ip_event_got_ip_t *e = data;
        snprintf(s_ip, sizeof s_ip, IPSTR, IP2STR(&e->ip_info.ip));
        ESP_LOGI(TAG, "拿到 IP: %s", s_ip);
        lcd_ui_ip(s_ip);
        lcd_ui_wifi(s_ssid);
        xEventGroupSetBits(s_evt, EVT_GOT_IP);
    }
}

/* 网络就绪后起云端长连接。放独立任务里：事件任务栈只有 3.5KB，
 * 在里面初始化 TLS 会爆栈。 */
static void services_task(void *arg)
{
    (void)arg;
    xEventGroupWaitBits(s_evt, EVT_GOT_IP, pdFALSE, pdFALSE, portMAX_DELAY);
    extern void netlog_start(void);
    netlog_start();

    uint8_t mac[6];
    esp_wifi_get_mac(WIFI_IF_STA, mac);
    memdiag_heap("cloud_client_start 之前");
    cloud_client_start(mac);
    ESP_LOGI(TAG, "云端服务已启动，堆=%u", (unsigned)esp_get_free_heap_size());
    memdiag_heap("cloud_client_start 之后（含 MQTT TLS 长连接）");

    char devid[24];
    snprintf(devid, sizeof devid, "%02x%02x%02x%02x%02x%02x",
             mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]);
    ota_start(devid);
    memdiag_heap("ota 定时器建好之后");
    coredump_start();
    memdiag_heap("coredump 检查之后");
    vTaskDelete(NULL);
}

/* OTA 试运行的健康检查：拿到 IP + MQTT 已连上，两项成立就算这版固件能用。
 *
 * 判据只能用**设备自己能决定**的条件。早先这里还要求「已配对」和「电视在
 * 拉流」，那是错的：电视关着、或者设备还没配对时，180 秒内必然过不了，
 * 而没通过就一直卡在 PENDING_VERIFY——之后 esp_ota_begin() 会直接返回
 * ROLLBACK_INVALID_STATE，连修复版都推不进来，哪天断电还会悄悄回滚。
 * 用户把电视关了一晚上，就能把整批设备锁死在这个状态里。
 *
 * 超时也必须有动作：主动 mark_invalid 并重启回滚，而不是打条日志就算了。
 * 「不确认」和「确认失败」在 bootloader 眼里不是一回事，前者会一直悬着。 */
static void health_task(void *arg)
{
    (void)arg;
    const int deadline_s = 180;
    for (int t = 1; t <= deadline_s; t++) {
        vTaskDelay(pdMS_TO_TICKS(1000));
        bool online = (xEventGroupGetBits(s_evt) & EVT_GOT_IP) != 0;
        if (online && cloud_connected()) {
            ESP_LOGI(TAG, "健康检查通过（%d 秒）：已联网 + 云端已连上", t);
            ota_mark_healthy();
            vTaskDelete(NULL);
        }
    }
    ESP_LOGE(TAG, "健康检查 %d 秒未通过，判定这版固件不可用，回滚上一槽", deadline_s);
    lcd_ui_cloud("新固件不可用，回滚");
    vTaskDelay(pdMS_TO_TICKS(1000));
    ota_rollback_now();   /* 不返回 */
    vTaskDelete(NULL);
}

/*
 * 守护：找电视 → 推送 → 盯着流。流空闲超过 REPUSH_IDLE_S（电视关机 /
 * 切走信号源 / IP 变了）就重新发现并重推；失败按 2 倍退避到 5 分钟，
 * 避免电视开着别的东西时被反复抢屏。
 */
static void supervisor_task(void *arg)
{
    (void)arg;
    char url[64];
    int backoff = CONFIG_STICKBOX_DLNA_REPUSH_IDLE_S;
    int64_t pushed_at = 0;
    int64_t last_stat = 0;

    while (1) {
        xEventGroupWaitBits(s_evt, EVT_GOT_IP, pdFALSE, pdFALSE, portMAX_DELAY);
        int64_t now = esp_timer_get_time();
        if (now - last_stat >= 60LL * 1000000) {
            last_stat = now;
            ESP_LOGI(TAG, "推流统计 frames=%lu clients=%d heap=%u min=%u",
                     (unsigned long)stream_frames_sent(), stream_clients(),
                     (unsigned)esp_get_free_heap_size(),
                     (unsigned)esp_get_minimum_free_heap_size());
        }
        int64_t last = stream_last_active_us();
        bool streaming = stream_clients() > 0 && now - last < 5000000;
        bool idle_long = !streaming &&
            (now - (last > pushed_at ? last : pushed_at)) >
                (int64_t)backoff * 1000000;

        if (streaming) {
            lcd_ui_stream("推流中", stream_frames_sent());
        } else if (pushed_at != 0) {
            lcd_ui_stream("已推送，等电视", stream_frames_sent());
        }

        if (pushed_at == 0 || idle_long) {
            lcd_ui_stream("正在搜索电视", 0);
            s_tv_found = dlna_find_tv(CONFIG_STICKBOX_DLNA_TV_IP, &s_tv);
            if (s_tv_found) {
                lcd_ui_tv(s_tv.name, s_tv.ip);
                snprintf(url, sizeof url, "http://%s:%d/live.avi", s_ip, STREAM_PORT);
                lcd_ui_stream("推送中", 0);
                if (dlna_push(&s_tv, url, "video/avi", "StickBox")) {
                    backoff = CONFIG_STICKBOX_DLNA_REPUSH_IDLE_S;
                    lcd_ui_stream("已推送，等电视", 0);
                } else {
                    backoff = backoff * 2 > 300 ? 300 : backoff * 2;
                    lcd_ui_stream("推送失败", 0);
                }
            } else {
                lcd_ui_tv("", "");
                backoff = backoff * 2 > 300 ? 300 : backoff * 2;
                lcd_ui_stream("未找到电视", 0);
            }
            pushed_at = esp_timer_get_time();
            ESP_LOGI(TAG, "下次检查在 %d 秒后", backoff);
        }
        vTaskDelay(pdMS_TO_TICKS(1000));
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

#if CONFIG_STICKBOX_OTA_SUICIDE
static void suicide_task(void *arg)
{
    (void)arg;
    vTaskDelay(pdMS_TO_TICKS(10000));
    ESP_LOGE(TAG, "OTA 回滚测试固件：现在 abort()");
    abort();
}
#endif

void app_main(void)
{
    ESP_LOGI(TAG, "=== StickBox 电视看板 ===");
    ota_boot_check();     /* 尽早：要先知道本次是不是 OTA 后的试运行 */
#if CONFIG_STICKBOX_OTA_SUICIDE
    /* 只为验证 OTA 回滚而存在的测试版本。
     *
     * 延迟崩溃而不是立刻 abort()：abort() 是 noreturn，编译器会把它之后的
     * 整个 app_main 当死代码消掉，固件从 2MB 缩到 214KB——那样既测不到真实
     * 大小的 OTA 下载，也测不到「启动正常、随后崩溃」这个更常见的故障形态。
     * 10 秒远小于健康检查的 180 秒窗口，所以这一版永远不会被标记为可用。 */
    xTaskCreate(suicide_task, "suicide", 2048, NULL, 1, NULL);
#endif
    ESP_LOGI(TAG, "堆@启动=%u", (unsigned)esp_get_free_heap_size());
    lcd_ui_init();
    ESP_LOGI(TAG, "堆@LCD后=%u", (unsigned)esp_get_free_heap_size());

    esp_err_t r = nvs_flash_init();
    if (r == ESP_ERR_NVS_NO_FREE_PAGES || r == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_ERROR_CHECK(nvs_flash_erase());
        ESP_ERROR_CHECK(nvs_flash_init());
    }
    s_evt = xEventGroupCreate();

    /* 开机按住 MENU 键 = 清空 Wi-Fi 配置重新配网 */
    gpio_config_t btn = { .pin_bit_mask = BIT64(GPIO_NUM_14),
                          .mode = GPIO_MODE_INPUT, .pull_up_en = GPIO_PULLUP_ENABLE };
    gpio_config(&btn);
    vTaskDelay(pdMS_TO_TICKS(50));
    if (gpio_get_level(GPIO_NUM_14) == 0) {
        ESP_LOGW(TAG, "检测到 MENU 键按下——清空 Wi-Fi 配置与设备密钥");
        lcd_ui_wifi("已清空配置");
        prov_erase();
        vTaskDelay(pdMS_TO_TICKS(1500));
    }

    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());

    /* 没有保存过凭据就先进配网门户（内部配好会自动重启，不会返回） */
    if (!prov_load(s_ssid, sizeof s_ssid, s_pass, sizeof s_pass)) {
        ESP_LOGW(TAG, "无 Wi-Fi 配置，进入配网模式");
        prov_portal_run();
    }
    ESP_LOGI(TAG, "已读取配置：SSID=\"%s\"", s_ssid);

    esp_netif_create_default_wifi_sta();
    wifi_init_config_t wc = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&wc));
    ESP_ERROR_CHECK(esp_event_handler_register(WIFI_EVENT, ESP_EVENT_ANY_ID, on_wifi, NULL));
    ESP_ERROR_CHECK(esp_event_handler_register(IP_EVENT, IP_EVENT_STA_GOT_IP, on_wifi, NULL));

    wifi_config_t sta = { 0 };
    strlcpy((char *)sta.sta.ssid, s_ssid, sizeof(sta.sta.ssid));
    strlcpy((char *)sta.sta.password, s_pass, sizeof(sta.sta.password));
    sta.sta.threshold.authmode = WIFI_AUTH_WPA_PSK;
    sta.sta.pmf_cfg.capable = true;
    sta.sta.pmf_cfg.required = false;
    sta.sta.ft_enabled = false;
    sta.sta.btm_enabled = false;
    sta.sta.rm_enabled = false;
    sta.sta.owe_enabled = false;
    sta.sta.scan_method = WIFI_ALL_CHANNEL_SCAN;
    sta.sta.sort_method = WIFI_CONNECT_AP_BY_SIGNAL;
    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
    ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_STA, &sta));
    ESP_ERROR_CHECK(esp_wifi_start());
    /* 持续发流，省电模式会让吞吐忽高忽低（probe 实测）。 */
    esp_wifi_set_ps(WIFI_PS_NONE);
    ESP_LOGI(TAG, "连接 Wi-Fi \"%s\"…", s_ssid);
    lcd_ui_wifi(s_ssid);

    /* 固件里只带一张**纯色底图**，没有任何演示数据。电视上要么是真实看板
     * （帧包到手之后），要么是功能性画面（配对码 / 连接中），不会出现任何
     * 看起来像数据却不是数据的东西。 */
    static frame_t frames[1];
    frames[0] = (frame_t){ blank_start, blank_end - blank_start };
    /* 配对码条带占 MCU 行 46..51；未配对时停在第 1 页显示它。 */
    memdiag_heap("stream_start 之前");
    stream_start(frames, 1, CONFIG_STICKBOX_DLNA_FPS, PAIR_ROW0, PAIR_ROWS);
    memdiag_heap("stream_start 之后（含带缓冲与页面解析）");

    sntp_start();
    xTaskCreate(services_task, "svc", 8192, NULL, 4, NULL);
    /* T8：实测栈余量 3.0KB，4096 仍留约 1KB。 */
    xTaskCreate(supervisor_task, "dlna_sup", 4096, NULL, 4, NULL);
    /* 只有 OTA 后的试运行才需要盯健康状态；平时这个任务根本不建，不占堆。 */
    if (ota_pending_verify())
        xTaskCreate(health_task, "ota_health", 3072, NULL, 2, NULL);

    EventBits_t bits = xEventGroupWaitBits(s_evt, EVT_GOT_IP, pdFALSE, pdFALSE,
                                           pdMS_TO_TICKS(30000));
    ESP_LOGI(TAG, "Wi-Fi 阶段结果: %s", (bits & EVT_GOT_IP) ? "已联网" : "30 秒超时，继续启动");
    ESP_LOGI(TAG, "启动完成，堆=%u", (unsigned)esp_get_free_heap_size());
    memdiag_heap("启动完成");
    memdiag_tasks("启动完成");
    memdiag_start();
}
