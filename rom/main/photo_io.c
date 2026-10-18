#include "photo_io.h"

#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"

static SemaphoreHandle_t s_mux;

void photo_io_init(void)
{
    if (!s_mux) s_mux = xSemaphoreCreateMutex();
}

bool photo_io_lock(uint32_t timeout_ms)
{
    if (!s_mux) return true;   /* 没锁可用：不能因此阻塞功能 */
    TickType_t ticks = timeout_ms == UINT32_MAX ? portMAX_DELAY
                                                : pdMS_TO_TICKS(timeout_ms);
    return xSemaphoreTake(s_mux, ticks) == pdTRUE;
}

void photo_io_unlock(void)
{
    if (s_mux) xSemaphoreGive(s_mux);
}
