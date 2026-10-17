#pragma once
#include <stdbool.h>

/* OTA 升级通道（T9）。
 *
 * 设备发出去之后只能靠这条路升级——没有第二次接串口的机会。所以这里的
 * 每个失败分支都必须「保持现状继续跑」，绝不能把设备留在半死状态。
 * 见 docs/PLAN.md T9、docs/PRODUCT.md 第 3 节。 */

/* 开机尽早调用：打印当前运行槽位，并判断本次是不是一次 OTA 后的试运行。
 * 是试运行就不做任何事——等 ota_mark_healthy()；超时没等到，重启自动回滚。 */
void ota_boot_check(void);

/* 本次启动是 OTA 之后的待验证试运行（PENDING_VERIFY）。 */
bool ota_pending_verify(void);

/* 健康检查通过，确认这版固件可用，取消回滚。
 * 幂等：非试运行状态下调用是空操作。 */
void ota_mark_healthy(void);

/* 起 OTA 检查任务：开机一次，之后每天一次。
 * 必须在 cloud_client_start() 之后调用（要用它的 api_base 与设备密钥）。 */
void ota_start(const char *devid);

/* 当前固件版本号，取自 app descriptor。心跳里一并上报。 */
const char *ota_version(void);
