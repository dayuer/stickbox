/*
 * 自带 tusb_config.h —— 这是绕过 esp_tinyusb 的关键。
 *
 * 为什么不用 esp_tinyusb：
 *   它的 device/esp_tinyusb/tinyusb_msc.c 无条件定义了全部 8 个 tud_msc_*
 *   回调（强符号，没有 weak，也没有任何 Kconfig 逃生口），而它的
 *   include/tusb_config.h 里是 `#define CFG_TUD_MSC CONFIG_TINYUSB_MSC_ENABLED`
 *   —— 想开 MSC 类就必然把那个存储层编进来，于是和我们自己的回调撞符号。
 *   它的存储层只支持 wear-levelling 分区 / SDMMC（IDF 6.0.4+ 才有
 *   esp_blockdev），都落不到"合成扇区"上。
 *
 *   所以这里直接依赖底层 espressif/tinyusb，自己提供配置、描述符、
 *   PHY 初始化和 tud_task() 任务。代价是多约 80 行胶水，换来完全的控制。
 */
#pragma once

#ifdef __cplusplus
extern "C" {
#endif

#define CFG_TUSB_MCU          OPT_MCU_ESP32S3
#define CFG_TUSB_OS           OPT_OS_FREERTOS
#define CFG_TUSB_RHPORT0_MODE (OPT_MODE_DEVICE | OPT_MODE_FULL_SPEED)

#define CFG_TUD_ENABLED   1
#define CFG_TUD_MAX_SPEED OPT_MODE_FULL_SPEED

#define CFG_TUSB_MEM_SECTION
#define CFG_TUSB_MEM_ALIGN __attribute__((aligned(4)))

#define CFG_TUD_ENDPOINT0_SIZE 64

#define CFG_TUD_CDC    0
#define CFG_TUD_MSC    1
#define CFG_TUD_HID    0
#define CFG_TUD_MIDI   0
#define CFG_TUD_VENDOR 0

/* 一次传输 8 个扇区。4KB 常驻内存换吞吐——这块板子没有 PSRAM，
 * 空闲堆只有 72~120KB，别再往上加。 */
#define CFG_TUD_MSC_EP_BUFSIZE 4096

#ifdef __cplusplus
}
#endif
