#pragma once

/* 崩溃回传（T10）。
 *
 * coredump 写 flash 早就开着了，但只能靠串口 idf.py coredump-info 读回来——
 * 别人家里的板子拿不到串口。这里把它传回服务端，否则现场故障没有任何线索。
 * 见 docs/PLAN.md T10。 */

/* 起回传任务：有待上传的 coredump 就传，传成功才擦。
 * 必须在 cloud_client_start() 之后调用（要用它的 api_base 与设备密钥）。 */
void coredump_start(void);

/* 本次启动的复位原因，心跳里带上。返回静态字符串，形如 "panic"、"poweron"。 */
const char *coredump_reset_reason(void);
