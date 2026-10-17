/*
 * OTA 升级通道（T9）。
 *
 * 设备发出去之后只能靠这条路升级，所以这里每个失败分支都是「保持现状继续跑」，
 * 绝不把设备留在半死状态。固件是开源的，二进制本身不是秘密，下载不鉴权；
 * 防篡改靠签名（sdkconfig.release 里的 SECURE_SIGNED_ON_UPDATE），不靠 secure boot
 * ——用户必须保留串口刷自己固件的权利。见 docs/PRODUCT.md 第 3 节。
 */
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "cJSON.h"
#include "esp_app_desc.h"
#include "esp_crt_bundle.h"
#include "esp_http_client.h"
#include "esp_https_ota.h"
#include "esp_log.h"
#include "esp_ota_ops.h"
#include "esp_system.h"

#include "cloud_client.h"
#include "ota.h"

static const char *TAG = "ota";

/* 开机检查一次，之后每天一次。 */
#define CHECK_FIRST_DELAY_S   120
#define CHECK_INTERVAL_S    86400

/* 健康检查窗口：这段时间内 ota_mark_healthy() 没被调用，重启就回滚。 */
#define HEALTH_TIMEOUT_S     180

/* 下载要再开一条 TLS（约 40KB）。T8 实测稳态空闲 139~141KB、历史最低 63.5KB，
 * 低于这个水位就本轮跳过——升级失败可以等明天，推流断了用户现在就看得见。 */
#define MIN_FREE_HEAP      90000

static bool s_pending_verify;
static char s_devid[24];

bool ota_pending_verify(void) { return s_pending_verify; }

const char *ota_version(void)
{
    const esp_app_desc_t *d = esp_app_get_description();
    return d ? d->version : "?";
}

void ota_boot_check(void)
{
    const esp_partition_t *run = esp_ota_get_running_partition();
    esp_ota_img_states_t st = ESP_OTA_IMG_UNDEFINED;
    if (run) esp_ota_get_state_partition(run, &st);

    s_pending_verify = (st == ESP_OTA_IMG_PENDING_VERIFY);
    ESP_LOGI(TAG, "运行槽位 %s，版本 %s，状态 %s",
             run ? run->label : "?", ota_version(),
             s_pending_verify ? "待验证（健康检查通过前不落定）" : "已确认");

    if (s_pending_verify)
        ESP_LOGW(TAG, "本次是 OTA 后试运行：%d 秒内没通过健康检查，重启将回滚",
                 HEALTH_TIMEOUT_S);
}

void ota_mark_healthy(void)
{
    if (!s_pending_verify) return;       /* 幂等：非试运行时是空操作 */
    esp_err_t e = esp_ota_mark_app_valid_cancel_rollback();
    if (e == ESP_OK) {
        s_pending_verify = false;
        ESP_LOGI(TAG, "健康检查通过，新固件已落定");
    } else {
        ESP_LOGE(TAG, "标记可用失败 %s，重启会回滚", esp_err_to_name(e));
    }
}

/* ---------------------------------------------------------------- 检查与下载 */

/* GET {api_base}/device/firmware，返回 HTTP 状态码，响应体写进 resp。 */
static int fetch_manifest(char *resp, size_t cap)
{
    char url[192];
    snprintf(url, sizeof url, "%s/device/firmware?dev=%s", cloud_api_base(), s_devid);

    esp_http_client_config_t cfg = {
        .url = url,
        .method = HTTP_METHOD_GET,
        .timeout_ms = 10000,
        .crt_bundle_attach = esp_crt_bundle_attach,
    };
    esp_http_client_handle_t h = esp_http_client_init(&cfg);
    if (!h) return -1;

    char auth[96];
    snprintf(auth, sizeof auth, "Bearer %s", cloud_devkey());
    esp_http_client_set_header(h, "Authorization", auth);

    int status = -1;
    if (esp_http_client_open(h, 0) == ESP_OK) {
        esp_http_client_fetch_headers(h);
        status = esp_http_client_get_status_code(h);
        int n = esp_http_client_read(h, resp, cap - 1);
        resp[n > 0 ? n : 0] = 0;
    }
    esp_http_client_cleanup(h);
    return status;
}

/* 下载并写入备用槽。成功返回 true（调用方随即重启）。 */
static bool download(const char *url)
{
    size_t free_heap = esp_get_free_heap_size();
    if (free_heap < MIN_FREE_HEAP) {
        ESP_LOGW(TAG, "空闲堆 %u < %d，本轮不升级（推流优先）",
                 (unsigned)free_heap, MIN_FREE_HEAP);
        return false;
    }

    ESP_LOGI(TAG, "开始下载 %s（堆 %u）", url, (unsigned)free_heap);
    esp_http_client_config_t http = {
        .url = url,
        .timeout_ms = 30000,
        .keep_alive_enable = true,
        .crt_bundle_attach = esp_crt_bundle_attach,
    };
    esp_https_ota_config_t cfg = { .http_config = &http };

    esp_err_t e = esp_https_ota(&cfg);
    if (e != ESP_OK) {
        /* 包括签名校验失败。写失败时 esp_https_ota 不会动 otadata，
         * 当前槽位不受影响，设备照常跑。 */
        ESP_LOGE(TAG, "升级失败 %s，继续用当前固件", esp_err_to_name(e));
        return false;
    }
    ESP_LOGI(TAG, "新固件已写入备用槽，重启进入试运行");
    return true;
}

static void check_once(void)
{
    char resp[512];
    int st = fetch_manifest(resp, sizeof resp);
    if (st != 200) {
        ESP_LOGI(TAG, "升级检查跳过（HTTP %d）", st);
        return;
    }
    cJSON *root = cJSON_Parse(resp);
    if (!root) { ESP_LOGW(TAG, "manifest 不是 JSON"); return; }

    const cJSON *ver = cJSON_GetObjectItemCaseSensitive(root, "version");
    const cJSON *url = cJSON_GetObjectItemCaseSensitive(root, "url");
    if (cJSON_IsString(ver) && cJSON_IsString(url) && ver->valuestring[0] &&
        url->valuestring[0]) {
        /* 不解析版本号大小，服务端说推什么就是什么——只要和当前跑的不一样。
         * 这样回退一个版本也走同一条路。 */
        if (strcmp(ver->valuestring, ota_version()) != 0) {
            ESP_LOGI(TAG, "有新版本 %s（当前 %s）", ver->valuestring, ota_version());
            if (download(url->valuestring)) {
                cJSON_Delete(root);
                vTaskDelay(pdMS_TO_TICKS(1000));
                esp_restart();
            }
        } else {
            ESP_LOGI(TAG, "已是最新 %s", ota_version());
        }
    }
    cJSON_Delete(root);
}

static void ota_task(void *arg)
{
    (void)arg;
    vTaskDelay(pdMS_TO_TICKS(CHECK_FIRST_DELAY_S * 1000));
    while (1) {
        check_once();
        vTaskDelay(pdMS_TO_TICKS(CHECK_INTERVAL_S * 1000));
    }
}

void ota_start(const char *devid)
{
    strlcpy(s_devid, devid, sizeof s_devid);
    /* 8192：和注册/换配对码同理，一次 TLS 握手 4096 太紧。 */
    xTaskCreate(ota_task, "ota", 8192, NULL, 2, NULL);
}
