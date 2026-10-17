/*
 * ESP32-S3-USB-OTG → 电视 USB 口：虚拟 U 盘探针。
 *
 * 要回答的三个问题（按重要性）：
 *   1. 电视认不认一支合成出来的 FAT32 U 盘？
 *   2. 盘上内容变了，怎样才能让电视重新扫描？热换 / 介质弹出 / USB 软拔插
 *      这三种手段里哪种在你那台机器上有效？
 *   3. 它到底按什么顺序读了哪些扇区？（/trace）
 *
 * 用法见 README.md。
 */

#include <string.h>

#include "driver/gpio.h"
#include "esp_event.h"
#include "esp_log.h"
#include "esp_netif.h"
#include "esp_system.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"
#include "freertos/task.h"
#include "nvs_flash.h"

#include "httpd.h"
#include "usbdev.h"
#include "vdisk.h"

static const char *TAG = "probe";

/* 板载 GPIO —— 与主工程 main/main.c 里的定义保持一致 */
#define PIN_USB_MODE_SEL     GPIO_NUM_18
#define PIN_USB_LIMIT_EN     GPIO_NUM_17
#define PIN_USB_DEV_VBUS_EN  GPIO_NUM_12
#define PIN_BATTERY_BOOST_EN GPIO_NUM_13
#define PIN_LED_GREEN        GPIO_NUM_15
#define PIN_LED_YELLOW       GPIO_NUM_16

static EventGroupHandle_t s_evt;
#define EVT_GOT_IP BIT0

/*
 * 切到 device 模式。跟主工程的 board_usb_host_power() 正好相反。
 *
 * ⚠️ 安全要点：插进电视时，VBUS 由电视提供。我们绝对不能同时往 DEV 口
 *    灌 5V —— 两个源对顶。所以 DEV_VBUS_EN 和 BOOST_EN 都必须是 0。
 *    LIMIT_EN 在主工程的注释里被证实同时是 host 口负载开关的使能，
 *    这里也置 0，让 host 口彻底不带电。
 */
static void board_usb_device_mode(void)
{
    const gpio_config_t io = {
        .pin_bit_mask = BIT64(PIN_USB_MODE_SEL) | BIT64(PIN_USB_LIMIT_EN) |
                        BIT64(PIN_USB_DEV_VBUS_EN) | BIT64(PIN_BATTERY_BOOST_EN) |
                        BIT64(PIN_LED_GREEN) | BIT64(PIN_LED_YELLOW),
        .mode = GPIO_MODE_OUTPUT,
    };
    ESP_ERROR_CHECK(gpio_config(&io));

    gpio_set_level(PIN_USB_DEV_VBUS_EN, 0);  /* 不往 DEV 口输出 5V */
    gpio_set_level(PIN_BATTERY_BOOST_EN, 0); /* 升压关掉 */
    gpio_set_level(PIN_USB_LIMIT_EN, 0);     /* host 口不带电 */
    gpio_set_level(PIN_USB_MODE_SEL, 0);     /* D+/D- 接到 DEV 连接器 */

    gpio_set_level(PIN_LED_GREEN, 0);
    gpio_set_level(PIN_LED_YELLOW, 0);
    ESP_LOGI(TAG, "USB 已切 device 模式（数据线在 DEV 口，不输出 VBUS）");
}

static void on_wifi(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        esp_wifi_connect();
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        esp_wifi_connect();
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        ip_event_got_ip_t *e = data;
        ESP_LOGW(TAG, "★ 控制面板： http://" IPSTR "/", IP2STR(&e->ip_info.ip));
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
    esp_wifi_set_max_tx_power(44); /* 跟主工程一致，削掉射频电流尖峰 */
}

static void blink_task(void *arg)
{
    (void)arg;
    bool on = false;
    while (1) {
        /* 绿灯：常亮=已被主机挂载，慢闪=没挂载 */
        if (usbdev_mounted()) {
            gpio_set_level(PIN_LED_GREEN, 1);
        } else {
            on = !on;
            gpio_set_level(PIN_LED_GREEN, on);
        }
        vTaskDelay(pdMS_TO_TICKS(500));
    }
}

void app_main(void)
{
    vTaskDelay(pdMS_TO_TICKS(4000)); /* DIAG */
    ESP_LOGI(TAG, "=== 虚拟 U 盘探针 ===");
    ESP_LOGI(TAG, "堆@启动=%u", (unsigned)esp_get_free_heap_size());

    esp_err_t r = nvs_flash_init();
    if (r == ESP_ERR_NVS_NO_FREE_PAGES || r == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_ERROR_CHECK(nvs_flash_erase());
        ESP_ERROR_CHECK(nvs_flash_init());
    }
    s_evt = xEventGroupCreate();

    board_usb_device_mode();
    wifi_start();

    /* 先起 USB，不等 Wi-Fi —— 电视一上电就该看到这支盘。
     * Wi-Fi 只是控制通道，断了也不影响"电视认不认盘"这个主问题。 */
    usbdev_start();
    xTaskCreate(blink_task, "blink", 2048, NULL, 2, NULL);
    ESP_LOGI(TAG, "堆@USB后=%u", (unsigned)esp_get_free_heap_size());

    if (xEventGroupWaitBits(s_evt, EVT_GOT_IP, pdFALSE, pdFALSE, pdMS_TO_TICKS(20000))) {
        probe_httpd_start();
    } else {
        ESP_LOGE(TAG, "20 秒没联网——USB 侧继续工作，但没有控制面板。"
                      "检查 idf.py menuconfig 里的 Wi-Fi 凭据。");
    }

    ESP_LOGI(TAG, "就绪。slide1=%u 字节，盘 %u 扇区，堆=%u",
             (unsigned)vd_slide_size(), (unsigned)vd_sector_count(),
             (unsigned)esp_get_free_heap_size());
}
