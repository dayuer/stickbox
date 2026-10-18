/*
 * 崩溃回传（T10）。
 *
 * 上传成功才擦镜像。失败就留着下次开机重试——宁可重传一次，也不能传丢了
 * 还把唯一的现场擦了。整个流程不阻塞启动：传不上去设备照常工作。
 *
 * v1.1.0 发版前的两条加固（开机堆最低点事故，见 coredump_policy.h）：
 *   1. 上传和取帧/清单走同一套：照片维护窗口 + net 锁 + 停流 + 堆 ≥90KB。
 *      之前它是唯一一个绕开这些、裸开 TLS 的网络任务，正好叠在照片首批帧上
 *   2. 同一镜像每天最多试 3 次（NVS 记账），服务端配置错不再放大成
 *      「每次开机一次 TLS + 40KB 上传」
 */
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "esp_app_desc.h"
#include "esp_core_dump.h"
#include "esp_crt_bundle.h"
#include "esp_http_client.h"
#include "esp_log.h"
#include "esp_partition.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "esp_heap_caps.h"
#include "esp_rom_crc.h"
#include "nvs.h"
#include "sdkconfig.h"

#include "cloud_client.h"
#include "coredump.h"
#include "coredump_policy.h"
#include "photo_io.h"
#include "photo_play.h"
#include "stream.h"

static const char *TAG = "coredump";

/* 开机后等一会儿再传：让 Wi-Fi、SNTP、MQTT 先各就各位，别和它们抢堆。 */
#define UPLOAD_DELAY_S 45

/* 一次读多少推给 HTTP。coredump 分区只有 64K，4K 一块足够，
 * 且不占堆（直接从 mmap 出来的地址拷）。 */
#define CHUNK 4096

/* 与 photo_sync.c 的 MIN_FREE_HEAP 同值：TLS 握手前要有这么多空闲堆。 */
#define MIN_FREE_HEAP   90000
#define HEAP_WAIT_MS    10000
#define NVS_NS          "cdump"
#define NVS_KEY         "st"
/* 测试构建里没有真镜像时用的假指纹。 */
#define FAKE_FP         0xFA4E0001u

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
        /* 堆上分配而不是栈上：4K 局部缓冲会把 8K 栈挤爆，而爆栈正好会
         * 覆盖掉 coredump，变成每次开机崩一次还查不到原因的循环。
         * 也不用 static——那 4KB 会常驻 .bss，而上传一年也未必发生一次。 */
        char *buf = malloc(CHUNK);
        if (!buf) ok = false;
        while (buf && off < size && ok) {
            size_t n = size - off < CHUNK ? size - off : CHUNK;
            /* addr 是分区内偏移，不是绝对地址。 */
            if (esp_partition_read(part, addr - part->address + off, buf, n) != ESP_OK) {
                ok = false;
                break;
            }
            ok = esp_http_client_write(h, buf, n) == (int)n;
            off += n;
        }
        free(buf);
        if (ok && buf) {
            esp_http_client_fetch_headers(h);
            status = esp_http_client_get_status_code(h);
        }
    }
    esp_http_client_cleanup(h);
    return status;
}

static esp_timer_handle_t s_timer;

/* 镜像指纹：大小 + 全量 CRC32。只读 flash，用 256B 栈缓冲，不占堆。 */
static uint32_t image_fp(size_t addr, size_t size)
{
    const esp_partition_t *part = esp_partition_find_first(
        ESP_PARTITION_TYPE_DATA, ESP_PARTITION_SUBTYPE_DATA_COREDUMP, NULL);
    if (!part) return (uint32_t)size;
    uint8_t b[256];
    uint32_t crc = 0;
    for (size_t off = 0; off < size; off += sizeof b) {
        size_t n = size - off < sizeof b ? size - off : sizeof b;
        if (esp_partition_read(part, addr - part->address + off, b, n) != ESP_OK) break;
        crc = esp_rom_crc32_le(crc, b, n);
    }
    return crc ^ (uint32_t)size;
}

static void state_load(cd_state_t *st)
{
    memset(st, 0, sizeof *st);
    nvs_handle_t h;
    if (nvs_open(NVS_NS, NVS_READONLY, &h) != ESP_OK) return;
    size_t len = sizeof *st;
    if (nvs_get_blob(h, NVS_KEY, st, &len) != ESP_OK || len != sizeof *st)
        memset(st, 0, sizeof *st);
    nvs_close(h);
}

static void state_save(const cd_state_t *st)
{
    nvs_handle_t h;
    if (nvs_open(NVS_NS, NVS_READWRITE, &h) != ESP_OK) return;
    nvs_set_blob(h, NVS_KEY, st, sizeof *st);
    nvs_commit(h);
    nvs_close(h);
}

static void state_clear(void)
{
    nvs_handle_t h;
    if (nvs_open(NVS_NS, NVS_READWRITE, &h) != ESP_OK) return;
    nvs_erase_key(h, NVS_KEY);
    nvs_commit(h);
    nvs_close(h);
}

/* 维护窗口里做一次上传。返回 HTTP 状态；
 * 返回 0 表示「没发出去」（锁/堆不够），不计入配额。 */
static int gated_upload(size_t addr, size_t size)
{
    /* 顺序与清单拉取一致：先让照片让出，再拿 net 锁（锁序 net → store，
     * 这里不拿 store，播放读卡不受影响但已暂停）。 */
    bool paused = photo_play_pause(4000);
    if (!paused) ESP_LOGW(TAG, "照片播放未及时让出，仍按 net 锁串行");
    if (!photo_io_net_lock(30000)) {
        photo_play_resume();
        ESP_LOGW(TAG, "拿不到 net 锁，本次不上传（不计次）");
        return 0;
    }
    stream_quiet(true);
    vTaskDelay(pdMS_TO_TICKS(1000));
    for (int i = 0; i < HEAP_WAIT_MS / 100 &&
                    esp_get_free_heap_size() < MIN_FREE_HEAP; i++)
        vTaskDelay(pdMS_TO_TICKS(100));
    int st = 0;
    if (esp_get_free_heap_size() < MIN_FREE_HEAP) {
        ESP_LOGW(TAG, "停流后堆仍只有 %u < %d，本次不上传（不计次）",
                 (unsigned)esp_get_free_heap_size(), MIN_FREE_HEAP);
    } else {
        ESP_LOGI(TAG, "上传 TLS 开始，堆 %u 最大块 %u",
                 (unsigned)esp_get_free_heap_size(),
                 (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL |
                                                            MALLOC_CAP_8BIT));
#if CONFIG_STICKBOX_COREDUMP_FAKE_STATUS
        (void)addr; (void)size;
        st = CONFIG_STICKBOX_COREDUMP_FAKE_STATUS;
        ESP_LOGW(TAG, "测试构建：不发网络，假装服务端返回 %d", st);
#else
        st = upload(addr, size);
        if (st == 0) st = -1;   /* 0 留给「没发出去」 */
#endif
        ESP_LOGI(TAG, "上传 TLS 结束，HTTP %d，堆 %u", st,
                 (unsigned)esp_get_free_heap_size());
    }
    stream_quiet(false);
    photo_io_net_unlock();
    photo_play_resume();
    return st;
}

static void coredump_task(void *arg)
{
    (void)arg;
    size_t addr = 0, size = 0;
    uint32_t fp;
    bool have = esp_core_dump_image_get(&addr, &size) == ESP_OK && size > 0;
#if CONFIG_STICKBOX_COREDUMP_FAKE_STATUS
    fp = have ? image_fp(addr, size) : FAKE_FP;
#else
    if (!have) vTaskDelete(NULL);
    fp = image_fp(addr, size);
#endif

    cd_state_t ps;
    state_load(&ps);
    uint32_t today = cd_day_of((int64_t)time(NULL));
    bool go = cd_should_try(&ps, fp, today);
    state_save(&ps);   /* 归一化后的状态（新镜像/新的一天）也要落盘 */
    if (!go) {
        ESP_LOGW(TAG, "同一镜像今天已试 %u 次（上限 %d），本次开机不上传",
                 ps.tries, CD_MAX_TRIES_PER_DAY);
        vTaskDelete(NULL);
    }

    int st = gated_upload(addr, size);
    if (st == 0) vTaskDelete(NULL);   /* 没发出去：不计次 */

    cd_class_t c = cd_classify(st);
    cd_note_attempt(&ps, st);
    if (c == CD_OK) {
#if CONFIG_STICKBOX_COREDUMP_FAKE_STATUS
        ESP_LOGW(TAG, "测试构建：成功也不擦镜像");
        state_save(&ps);
#else
        /* 只有确认服务端收下了才擦。传丢了还把唯一的现场擦掉，
         * 这个故障就永远查不了了。 */
        esp_core_dump_image_erase();
        state_clear();
        ESP_LOGI(TAG, "崩溃镜像已上传并擦除");
#endif
    } else {
        state_save(&ps);
        ESP_LOGW(TAG, "上传失败 HTTP %d（%s），今天第 %u/%d 次，保留镜像",
                 st, c == CD_PERMANENT ? "永久，配额用完前不会好" : "可重试",
                 ps.tries, CD_MAX_TRIES_PER_DAY);
    }
    vTaskDelete(NULL);
}

/* 和 OTA 同理：用定时器代替常驻任务。没有待传镜像时（绝大多数开机）
 * 根本不会创建任务，一个字节的栈都不占。 */
/* 同 ota：回调跑在 2048 栈的 esp_timer 任务里，只做一次 xTaskCreate。
 * 镜像存在与否在 coredump_start() 里已经查过了，任务里会再查一次，
 * 不必在回调里读 flash。 */
static void on_upload_timer(void *arg)
{
    (void)arg;
    xTaskCreate(coredump_task, "coredump", 8192, NULL, 2, NULL);
}

void coredump_start(void)
{
    ESP_LOGI(TAG, "本次复位原因：%s", coredump_reset_reason());
    size_t addr = 0, size = 0;
    if (esp_core_dump_image_get(&addr, &size) != ESP_OK || size == 0) {
#if CONFIG_STICKBOX_COREDUMP_FAKE_STATUS
        ESP_LOGW(TAG, "测试构建：没有镜像也模拟一次上传（假状态 %d）",
                 CONFIG_STICKBOX_COREDUMP_FAKE_STATUS);
        size = 0;
#else
        ESP_LOGI(TAG, "没有待上传的崩溃镜像");
        return;    /* 常态：连定时器都不建 */
#endif
    }
    ESP_LOGW(TAG, "发现崩溃镜像 %u 字节，%d 秒后上传", (unsigned)size, UPLOAD_DELAY_S);
    const esp_timer_create_args_t a = { .callback = on_upload_timer, .name = "coredump" };
    if (esp_timer_create(&a, &s_timer) == ESP_OK)
        esp_timer_start_once(s_timer, (uint64_t)UPLOAD_DELAY_S * 1000000ULL);
}
