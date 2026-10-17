/*
 * 只读 MSC 设备 + 三种"逼主机重扫"的手段。
 *
 * ⚠️ 本文件里有三处 API 没法在容器里编译验证，已逐个标注 [待验证]。
 *    跑 tools/check-api.sh 可以在 idf.py reconfigure 之后一键核对。
 */

#include "usbdev.h"

#include <stdio.h>
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "esp_log.h"
#include "esp_private/usb_phy.h" /* [待验证] 私有头，IDF 5.x 的 esp_tinyusb 自己也用它 */
#include "tusb.h"

#include "vdisk.h"

static const char *TAG = "usbdev";

/* ── 描述符 ── */
enum { ITF_MSC = 0, ITF_TOTAL };
#define EPNUM_MSC_OUT 0x01
#define EPNUM_MSC_IN  0x81
#define DESC_CFG_LEN  (TUD_CONFIG_DESC_LEN + TUD_MSC_DESC_LEN)

static const tusb_desc_device_t s_desc_dev = {
    .bLength         = sizeof(tusb_desc_device_t),
    .bDescriptorType = TUSB_DESC_DEVICE,
    .bcdUSB          = 0x0200,
    /* 类信息放在接口上（MSC 的惯例），设备级留 0 —— 电视靠接口的
     * 08/06/50 (MSC/SCSI/BOT) 认出这是个 U 盘，跟 VID/PID 无关。 */
    .bDeviceClass    = 0x00,
    .bDeviceSubClass = 0x00,
    .bDeviceProtocol = 0x00,
    .bMaxPacketSize0 = CFG_TUD_ENDPOINT0_SIZE,
    .idVendor        = 0x303A, /* Espressif */
    .idProduct       = 0x4002,
    .bcdDevice       = 0x0100,
    .iManufacturer   = 0x01,
    .iProduct        = 0x02,
    .iSerialNumber   = 0x03,
    .bNumConfigurations = 0x01,
};

static const uint8_t s_desc_cfg[] = {
    /* 属性 0x00 = 总线供电、不支持远程唤醒；100 = 200mA。
     * 我们确实靠电视供电，所以别谎报 self-powered。 */
    TUD_CONFIG_DESCRIPTOR(1, ITF_TOTAL, 0, DESC_CFG_LEN, 0x00, 100),
    TUD_MSC_DESCRIPTOR(ITF_MSC, 0, EPNUM_MSC_OUT, EPNUM_MSC_IN, 64),
};

const uint8_t *tud_descriptor_device_cb(void)
{
    return (const uint8_t *)&s_desc_dev;
}

const uint8_t *tud_descriptor_configuration_cb(uint8_t index)
{
    (void)index;
    return s_desc_cfg;
}

/* ── 字符串描述符：序列号可变 ──
 * 很多主机按 (VID,PID,序列号) 缓存卷信息。序列号一变，电视就认为
 * 这是另一支从没见过的 U 盘 —— 这是软拔插还不够时的最后一招。 */
static char     s_serial[17] = "ESP32VDISK000000";
static bool     s_serial_vary;
static uint32_t s_enum_n;
static uint16_t s_str[34];

static void serial_bump(void)
{
    s_enum_n++;
    if (s_serial_vary) snprintf(s_serial, sizeof s_serial, "ESP32VDISK%06u", (unsigned)s_enum_n);
}

const uint16_t *tud_descriptor_string_cb(uint8_t index, uint16_t langid)
{
    (void)langid;
    const char *s;
    switch (index) {
    case 0: /* 语言 ID */
        s_str[1] = 0x0409;
        s_str[0] = (uint16_t)((TUSB_DESC_STRING << 8) | 4);
        return s_str;
    case 1: s = "Espressif";         break;
    case 2: s = "ESP32 VDISK PROBE"; break;
    case 3: s = s_serial;            break;
    default: return NULL;
    }
    size_t n = strlen(s);
    if (n > 32) n = 32;
    for (size_t i = 0; i < n; i++) s_str[1 + i] = (uint16_t)s[i]; /* ASCII → UTF-16LE */
    s_str[0] = (uint16_t)((TUSB_DESC_STRING << 8) | (2 * n + 2));
    return s_str;
}

/* ── 介质状态 ── */
static volatile bool s_absent; /* 介质不在位（模拟读卡器拔卡） */
static volatile bool s_attn;   /* 待上报一次 UNIT ATTENTION */

void tud_mount_cb(void)
{
    ESP_LOGI(TAG, "主机已挂载（序列号 %s）", s_serial);
}
void tud_umount_cb(void) { ESP_LOGW(TAG, "主机已卸载"); }
void tud_suspend_cb(bool remote_wakeup_en)
{
    (void)remote_wakeup_en;
    ESP_LOGW(TAG, "总线挂起");
}
void tud_resume_cb(void) { ESP_LOGI(TAG, "总线恢复"); }

bool usbdev_mounted(void) { return tud_mounted(); }

/* ── MSC 回调 ── */
uint8_t tud_msc_get_maxlun_cb(void) { return 1; }

void tud_msc_inquiry_cb(uint8_t lun, uint8_t vendor_id[8], uint8_t product_id[16],
                        uint8_t product_rev[4])
{
    (void)lun;
    memset(vendor_id, ' ', 8);
    memset(product_id, ' ', 16);
    memset(product_rev, ' ', 4);
    memcpy(vendor_id, "ESP32", 5);
    memcpy(product_id, "VDISK PROBE", 11);
    memcpy(product_rev, "1.0", 3);
}

bool tud_msc_test_unit_ready_cb(uint8_t lun)
{
    if (s_absent) {
        /* 0x3A/0x00 = MEDIUM NOT PRESENT */
        tud_msc_set_sense(lun, SCSI_SENSE_NOT_READY, 0x3A, 0x00);
        return false;
    }
    if (s_attn) {
        /* 0x28/0x00 = NOT READY TO READY CHANGE, MEDIUM MAY HAVE CHANGED.
         * 这是 SCSI 里"介质换了，请重读"的标准信号，读卡器就是这么干的。
         * 只报一次，之后恢复正常。 */
        s_attn = false;
        tud_msc_set_sense(lun, SCSI_SENSE_UNIT_ATTENTION, 0x28, 0x00);
        ESP_LOGI(TAG, "已上报 UNIT ATTENTION（介质可能已更换）");
        return false;
    }
    return true;
}

void tud_msc_capacity_cb(uint8_t lun, uint32_t *block_count, uint16_t *block_size)
{
    (void)lun;
    *block_count = vd_sector_count();
    *block_size  = VD_SECTOR_SIZE;
}

bool tud_msc_start_stop_cb(uint8_t lun, uint8_t power_condition, bool start, bool load_eject)
{
    (void)lun;
    (void)power_condition;
    if (load_eject) {
        s_absent = !start;
        ESP_LOGI(TAG, "主机请求 %s 介质", start ? "装入" : "弹出");
    }
    return true;
}

bool tud_msc_is_writable_cb(uint8_t lun)
{
    (void)lun;
    return false; /* 整盘写保护：内容是合成的，写进来无处可去 */
}

int32_t tud_msc_read10_cb(uint8_t lun, uint32_t lba, uint32_t offset, void *buffer,
                          uint32_t bufsize)
{
    (void)lun;
    if (s_absent) return -1;
    if (offset % VD_SECTOR_SIZE) return -1;
    uint32_t nsec = bufsize / VD_SECTOR_SIZE;
    if (nsec == 0) return -1;
    vd_read(lba + offset / VD_SECTOR_SIZE, (uint8_t *)buffer, nsec);
    return (int32_t)(nsec * VD_SECTOR_SIZE);
}

int32_t tud_msc_write10_cb(uint8_t lun, uint32_t lba, uint32_t offset, uint8_t *buffer,
                           uint32_t bufsize)
{
    (void)lba;
    (void)offset;
    (void)buffer;
    (void)bufsize;
    /* 0x27/0x00 = WRITE PROTECTED */
    tud_msc_set_sense(lun, SCSI_SENSE_DATA_PROTECT, 0x27, 0x00);
    return -1;
}

int32_t tud_msc_scsi_cb(uint8_t lun, uint8_t const scsi_cmd[16], void *buffer, uint16_t bufsize)
{
    (void)buffer;
    (void)bufsize;
    ESP_LOGD(TAG, "未处理的 SCSI 命令 0x%02x", scsi_cmd[0]);
    /* 0x20/0x00 = INVALID COMMAND OPERATION CODE */
    tud_msc_set_sense(lun, SCSI_SENSE_ILLEGAL_REQUEST, 0x20, 0x00);
    return -1;
}

/* ── 三种重扫手段 ── */
void usbdev_swap_hot(int slide)
{
    vd_slide_commit_unsafe(slide);
}

void usbdev_eject_cycle(uint32_t absent_ms)
{
    ESP_LOGW(TAG, "介质弹出 %ums…", (unsigned)absent_ms);
    s_absent = true;
    vTaskDelay(pdMS_TO_TICKS(absent_ms));
    vd_slide_commit();
    s_absent = false;
    s_attn   = true;
    ESP_LOGW(TAG, "介质已装回，等电视来读");
}

void usbdev_replug(uint32_t down_ms)
{
    ESP_LOGW(TAG, "USB 软拔出 %ums…", (unsigned)down_ms);
    tud_disconnect(); /* [待验证] TinyUSB 核心 API，撤掉 D+ 上拉 */
    vTaskDelay(pdMS_TO_TICKS(down_ms));
    vd_slide_commit();
    serial_bump();
    tud_connect();
    ESP_LOGW(TAG, "USB 已重新接入，序列号 %s", s_serial);
}

void usbdev_set_serial_varying(bool on)
{
    s_serial_vary = on;
    if (!on) snprintf(s_serial, sizeof s_serial, "ESP32VDISK000000");
    ESP_LOGI(TAG, "可变序列号：%s", on ? "开" : "关");
}
bool usbdev_serial_varying(void) { return s_serial_vary; }
const char *usbdev_serial(void) { return s_serial; }

/* ── 启动 ── */
static usb_phy_handle_t s_phy;

static void tusb_task(void *arg)
{
    (void)arg;
    while (1) tud_task(); /* 内部会阻塞在队列上，不是忙等 */
}

void usbdev_start(void)
{
    /* [待验证] 字段名来自 IDF 5.x 的 esp_private/usb_phy.h；
     * esp_tinyusb 自己也是这么初始化的。 */
    usb_phy_config_t pc = {
        .controller = USB_PHY_CTRL_OTG,
        .target     = USB_PHY_TARGET_INT,
        .otg_mode   = USB_OTG_MODE_DEVICE,
        .otg_speed  = USB_PHY_SPEED_UNDEFINED,
    };
    ESP_LOGW(TAG, "DIAG: usb_new_phy ...");
    esp_err_t pe = usb_new_phy(&pc, &s_phy);
    ESP_LOGW(TAG, "DIAG: usb_new_phy -> %s", esp_err_to_name(pe));
    ESP_ERROR_CHECK(pe);

    /* 用 tud_init(0) 而不是 tusb_init()：后者的签名在 TinyUSB 0.17
     * 前后变过（无参 → (rhport, init_cfg)），tud_init 一直是稳定的。 */
    ESP_LOGW(TAG, "DIAG: tud_init ...");
    if (!tud_init(0)) {
        ESP_LOGE(TAG, "tud_init 失败");
        return;
    }
    ESP_LOGW(TAG, "DIAG: tud_init ok");
    xTaskCreate(tusb_task, "tusb", 4096, NULL, 5, NULL);
    ESP_LOGI(TAG, "MSC 就绪：%u 扇区 / %u MiB，只读",
             (unsigned)vd_sector_count(),
             (unsigned)(vd_sector_count() / 2048u));
}
