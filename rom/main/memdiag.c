/*
 * 内存对账（诊断用）。见 memdiag.h 里为什么需要它。
 *
 * 诊断构建自己也要占内存（FreeRTOS 的 trace facility、任务数组、这个任务的
 * 栈），所以它打出来的数会**略低于**正式构建。对账看的是各项之间的差值和
 * 分布，不是绝对值。
 */
/* sdkconfig.h 必须在 #if 之前引入——否则 CONFIG_STICKBOX_MEMDIAG 在求值时
 * 还没定义，#if 按 0 处理，整个文件静悄悄编译成空壳，而构建照样成功。 */
#include "sdkconfig.h"

#include "memdiag.h"

#if CONFIG_STICKBOX_MEMDIAG

#include <stdlib.h>
#include <string.h>

#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

static const char *TAG = "memdiag";

void memdiag_heap(const char *label)
{
    multi_heap_info_t in;
    heap_caps_get_info(&in, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    ESP_LOGW(TAG, "[%s] 空闲=%u 最大块=%u 历史最低=%u",
             label, (unsigned)in.total_free_bytes,
             (unsigned)in.largest_free_block,
             (unsigned)in.minimum_free_bytes);
}

void memdiag_tasks(const char *label)
{
    UBaseType_t n = uxTaskGetNumberOfTasks();
    TaskStatus_t *st = calloc(n, sizeof *st);
    if (!st) {
        ESP_LOGE(TAG, "[%s] 任务表分配不出来（这本身就是信号）", label);
        return;
    }
    n = uxTaskGetSystemState(st, n, NULL);
    ESP_LOGW(TAG, "[%s] 共 %u 个任务：", label, (unsigned)n);
    for (UBaseType_t i = 0; i < n; i++) {
        /* usStackHighWaterMark 是「这个任务用到过的最深处离栈顶还剩多少」，
         * 单位是字（4 字节）。余量越小越危险；余量很大说明栈开大了，
         * 那部分堆是白占的。 */
        ESP_LOGW(TAG, "  %-14s 栈余量最低=%u 字节  栈底=%p  状态=%d",
                 st[i].pcTaskName,
                 (unsigned)(st[i].usStackHighWaterMark * sizeof(StackType_t)),
                 (void *)st[i].pxStackBase,
                 (int)st[i].eCurrentState);
    }
    free(st);
}

/* 重活必须离开 esp_timer 任务。
 *
 * T8 把 ESP_TIMER_TASK_STACK_SIZE 收到了 2048，而 uxTaskGetSystemState 要
 * 分配任务数组、再逐个格式化日志（netlog 的 vprintf 钩子还会在调用者栈上
 * 再拼一次），直接在回调里干会把那 2048 吃到只剩 148 字节——离栈溢出就差
 * 一点，而且是诊断代码把被诊断的系统推到崩溃边缘。
 *
 * 回调里只做一次 xTaskCreate，别的什么都不做。 */
static void dump_task(void *arg)
{
    const char *label = arg;
    memdiag_heap(label);
    memdiag_tasks(label);
    vTaskDelete(NULL);
}

static void on_tick(void *arg)
{
    xTaskCreate(dump_task, "memdump", 4096, arg, 1, NULL);
}

void memdiag_start(void)
{
    static esp_timer_handle_t t60, t120;
    static const char L60[] = "60秒", L120[] = "120秒";
    esp_timer_create_args_t a = { .callback = on_tick, .name = "memdiag" };

    a.arg = (void *)L60;
    if (esp_timer_create(&a, &t60) == ESP_OK) esp_timer_start_once(t60, 60ULL * 1000000);
    a.arg = (void *)L120;
    if (esp_timer_create(&a, &t120) == ESP_OK) esp_timer_start_once(t120, 120ULL * 1000000);
    ESP_LOGW(TAG, "内存对账已开启：60 秒、120 秒各打一次");
}

#else  /* 关掉时全部编译成空 */

void memdiag_heap(const char *label) { (void)label; }
void memdiag_tasks(const char *label) { (void)label; }
void memdiag_start(void) { }

#endif
