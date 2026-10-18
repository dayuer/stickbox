/* microSD 适配层（M1）：SDMMC 4-bit→1-bit、4KB DMA 缓冲、裸扇区 IO。 */
#include "photo_sd.h"

#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "driver/sdmmc_host.h"
#include "sdmmc_cmd.h"
#include "esp_heap_caps.h"
#include "esp_log.h"

static const char *TAG = "photosd";

/* BSP（espressif__esp32_s3_usb_otg）4-bit SDMMC 引脚；与 USB/LCD/按键无重叠。 */
#define SD_D0   37
#define SD_D1   38
#define SD_D2   33
#define SD_D3   34
#define SD_CMD  35
#define SD_CLK  36

static sdmmc_card_t s_card_mem;
static sdmmc_card_t *s_card;
static bool s_inited;
static int s_width;
static uint8_t *s_buf;
static photostore_t s_store;

static bool sd_read(void *ctx, uint32_t sector, void *buf, uint32_t nsec)
{
    (void)ctx;
    if (!s_inited) return false;
    return sdmmc_read_sectors(s_card, buf, sector, nsec) == ESP_OK;
}

/* 卡已初始化但超级块读不出来 = fault，不能当成“没插卡”或“未初始化”。
 * 后者会被上层引导去格式化，这是绝对不行的。 */
static photo_state_t mapped_state(void)
{
    photo_state_t st = photostore_state(&s_store);
    return st == PHOTO_ABSENT ? PHOTO_FAULT : st;
}

static bool sd_write(void *ctx, uint32_t sector, const void *buf, uint32_t nsec)
{
    (void)ctx;
    if (!s_inited) return false;
    return sdmmc_write_sectors(s_card, buf, sector, nsec) == ESP_OK;
}

static esp_err_t bus_init(int width)
{
    sdmmc_host_t host = SDMMC_HOST_DEFAULT();
    host.flags = (width == 4) ? SDMMC_HOST_FLAG_4BIT : SDMMC_HOST_FLAG_1BIT;
    host.max_freq_khz = SDMMC_FREQ_HIGHSPEED;
    esp_err_t err = sdmmc_host_init();
    if (err != ESP_OK) return err;

    sdmmc_slot_config_t slot = SDMMC_SLOT_CONFIG_DEFAULT();
    slot.width = width;
    slot.clk = SD_CLK;
    slot.cmd = SD_CMD;
    slot.d0 = SD_D0;
    if (width == 4) { slot.d1 = SD_D1; slot.d2 = SD_D2; slot.d3 = SD_D3; }
    slot.flags |= SDMMC_SLOT_FLAG_INTERNAL_PULLUP;
    err = sdmmc_host_init_slot(host.slot, &slot);
    if (err != ESP_OK) { sdmmc_host_deinit(); return err; }

    err = sdmmc_card_init(&host, &s_card_mem);
    if (err == ESP_OK) {
        s_card = &s_card_mem;
        s_inited = true;
        s_width = width;
    } else {
        sdmmc_host_deinit();
    }
    return err;
}

photo_state_t photo_sd_mount(void)
{
    if (s_inited) return photostore_state(&s_store);

    esp_err_t err = bus_init(4);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "4-bit 失败（%s），退 1-bit", esp_err_to_name(err));
        err = bus_init(1);
    }
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "SD 初始化失败：%s", esp_err_to_name(err));
        return PHOTO_ABSENT;
    }
    if (!s_buf) s_buf = heap_caps_malloc(4096, MALLOC_CAP_DMA | MALLOC_CAP_8BIT);
    if (!s_buf) {
        ESP_LOGE(TAG, "4KB DMA 缓冲分配失败");
        sdmmc_host_deinit();
        s_inited = false;
        s_card = NULL;
        return PHOTO_ABSENT;
    }

    photo_io_t io = {
        .ctx = NULL, .read = sd_read, .write = sd_write,
        .sector_count = s_card->csd.capacity,
    };
    photostore_open(&s_store, &io, s_buf, 4096);
    photo_state_t st = mapped_state();
    ESP_LOGW(TAG, "挂载完成：%s，%d-bit，容量 %llu MB，状态 %d，fmt_mode %d",
             s_card->cid.name, s_width,
             ((unsigned long long)s_card->csd.capacity) * 512ULL / (1024 * 1024),
             (int)st, (int)photostore_format_mode(&s_store));
    return st;
}

void photo_sd_unmount(void)
{
    if (s_inited) sdmmc_host_deinit();
    s_inited = false;
    s_card = NULL;
    s_width = 0;
    if (s_buf) { free(s_buf); s_buf = NULL; }
    photostore_close(&s_store);
    ESP_LOGI(TAG, "已卸载");
}

bool photo_sd_mounted(void) { return s_inited; }

photo_state_t photo_sd_state(void)
{
    return s_inited ? mapped_state() : PHOTO_ABSENT;
}

bool photo_sd_format(bool authorized, bool force)
{
    if (!authorized) {
        ESP_LOGW(TAG, "没有 init_token，拒绝格式化");
        return false;
    }
    photo_state_t st = photo_sd_mount();
    if (st == PHOTO_ABSENT) return false;
    photo_format_mode_t mode = photostore_format_mode(&s_store);
    ESP_LOGW(TAG, "格式化请求：mode=%d force=%d", (int)mode, force ? 1 : 0);
    if (!photostore_format(&s_store, force)) return false;
    ESP_LOGW(TAG, "格式化完成");
    return true;
}

photo_format_mode_t photo_sd_format_mode(void)
{
    if (!s_inited) return PHOTO_FMT_UNAVAILABLE;
    return photostore_format_mode(&s_store);
}

photostore_t *photo_sd_store(void)
{
    if (!s_inited || photostore_state(&s_store) != PHOTO_READY) return NULL;
    return &s_store;
}
