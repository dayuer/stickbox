#include "photo_io.h"

#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"

static SemaphoreHandle_t s_net;
static SemaphoreHandle_t s_store;

void photo_io_init(void)
{
    if (!s_net) s_net = xSemaphoreCreateMutex();
    if (!s_store) s_store = xSemaphoreCreateMutex();
}

static bool take(SemaphoreHandle_t m, uint32_t timeout_ms)
{
    if (!m) return true;   /* 没锁可用：不能因此阻塞功能 */
    TickType_t ticks = timeout_ms == UINT32_MAX ? portMAX_DELAY
                                                : pdMS_TO_TICKS(timeout_ms);
    return xSemaphoreTake(m, ticks) == pdTRUE;
}

bool photo_io_net_lock(uint32_t timeout_ms) { return take(s_net, timeout_ms); }
void photo_io_net_unlock(void) { if (s_net) xSemaphoreGive(s_net); }

bool photo_io_store_lock(uint32_t timeout_ms) { return take(s_store, timeout_ms); }
void photo_io_store_unlock(void) { if (s_store) xSemaphoreGive(s_store); }
