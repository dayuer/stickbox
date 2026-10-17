/*
 * 崩溃回传（T10）。
 *
 * 上传成功才擦镜像。失败就留着下次开机重试——宁可重传一次，也不能传丢了
 * 还把唯一的现场擦了。整个流程不阻塞启动：传不上去设备照常工作。
 */
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "esp_app_desc.h"
#include "esp_core_dump.h"
#include "esp_crt_bundle.h"
#include "esp_http_client.h"
#include "esp_log.h"
#include "esp_partition.h"
#include "esp_system.h"

#include "cloud_client.h"
#include "coredump.h"

static const char *TAG = "coredump";

/* 开机后等一会儿再传：让 Wi-Fi、SNTP、MQTT 先各就各位，别和它们抢堆。 */
#define UPLOAD_DELAY_S 45

/* 一次读多少推给 HTTP。coredump 分区只有 64K，4K 一块足够，
 * 且不占堆（直接从 mmap 出来的地址拷）。 */
#define CHUNK 4096

const char *coredump_reset_reason(void)
{
    switch (esp_reset_reason()) {
    case ESP_RST_POWERON:  return "poweron";
    case ESP_RST_SW:       return "sw";
    case ESP_RST_PANIC:    return "panic";
    case ESP_RST_INT_WDT:  return "int_wdt";
    case ESP_RST_TASK_WDT: return "task_wdt";
    case ESP_RST_WDT:      return "wdt";
    case ESP_RST_BROWNOUT: return "brownout";
    case ESP_RST_DEEPSLEEP:return "deepsleep";
    case ESP_RST_EXT:      return "ext";
    default:               return "unknown";
    }
}

/* 把 coredump 分区里的镜像流式 POST 上去。返回 HTTP 状态码。
 *
 * 流式是必须的：堆里放不下整份镜像，而且上传时推流还在跑。 */
static int upload(size_t addr, size_t size)
{
    const esp_partition_t *part = esp_partition_find_first(
        ESP_PARTITION_TYPE_DATA, ESP_PARTITION_SUBTYPE_DATA_COREDUMP, NULL);
    if (!part) return -1;

    char url[192];
    snprintf(url, sizeof url, "%s/device/coredump", cloud_api_base());

    esp_http_client_config_t cfg = {
        .url = url,
        .method = HTTP_METHOD_POST,
        .timeout_ms = 30000,
        .buffer_size = 1024,
        .crt_bundle_attach = esp_crt_bundle_attach,
    };
    esp_http_client_handle_t h = esp_http_client_init(&cfg);
    if (!h) return -1;

    char auth[96];
    snprintf(auth, sizeof auth, "Bearer %s", cloud_devkey());
    esp_http_client_set_header(h, "Authorization", auth);
    esp_http_client_set_header(h, "Content-Type", "application/octet-stream");

    /* 固件版本和复位原因走头部：服务端不解 ELF，光有镜像没有版本号
     * 是没法还原符号的。 */
    const esp_app_desc_t *d = esp_app_get_description();
    esp_http_client_set_header(h, "X-Stickbox-Version", d ? d->version : "?");
    esp_http_client_set_header(h, "X-Stickbox-Reset", coredump_reset_reason());

    int status = -1;
    if (esp_http_client_open(h, (int)size) == ESP_OK) {
        size_t off = 0;
        bool ok = true;
        /* static：这个任务的栈要同时扛一次 TLS 握手，4K 的局部缓冲会把
         * 8K 栈挤爆。爆栈又正好会覆盖掉 coredump，变成每次开机 45 秒崩
         * 一次的循环——而且现场拿不到任何线索。 */
        static char buf[CHUNK];
        while (off < size && ok) {
            size_t n = size - off < CHUNK ? size - off : CHUNK;
            /* addr 是分区内偏移，不是绝对地址。 */
            if (esp_partition_read(part, addr - part->address + off, buf, n) != ESP_OK) {
                ok = false;
                break;
            }
            ok = esp_http_client_write(h, buf, n) == (int)n;
            off += n;
        }
        if (ok) {
            esp_http_client_fetch_headers(h);
            status = esp_http_client_get_status_code(h);
        }
    }
    esp_http_client_cleanup(h);
    return status;
}

static void coredump_task(void *arg)
{
    (void)arg;
    ESP_LOGI(TAG, "本次复位原因：%s", coredump_reset_reason());

    size_t addr = 0, size = 0;
    if (esp_core_dump_image_get(&addr, &size) != ESP_OK || size == 0) {
        ESP_LOGI(TAG, "没有待上传的崩溃镜像");
        vTaskDelete(NULL);
    }
    ESP_LOGW(TAG, "发现崩溃镜像 %u 字节，%d 秒后上传", (unsigned)size, UPLOAD_DELAY_S);
    vTaskDelay(pdMS_TO_TICKS(UPLOAD_DELAY_S * 1000));

    int st = upload(addr, size);
    if (st == 200 || st == 201 || st == 204) {
        /* 只有确认服务端收下了才擦。传丢了还把唯一的现场擦掉，
         * 这个故障就永远查不了了。 */
        esp_core_dump_image_erase();
        ESP_LOGI(TAG, "崩溃镜像已上传并擦除");
    } else {
        ESP_LOGW(TAG, "上传失败 HTTP %d，保留镜像下次开机重试", st);
    }
    vTaskDelete(NULL);
}

void coredump_start(void)
{
    /* 8192：一次 TLS 握手（和注册/换配对码同量级，那两个用的就是 8192），
     * 外加 upload() 里 CHUNK 那 4K 的栈上缓冲。 */
    xTaskCreate(coredump_task, "coredump", 8192, NULL, 2, NULL);
}
