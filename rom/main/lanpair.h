#pragma once
#include <stdbool.h>
#include <stddef.h>

/* 局域网配对（#28）。无常驻任务、无常驻堆：
 *  - lanpair_http(): 由 stream.c 的临时 client_task 在收到 GET /pair 时调用；
 *  - lanpair_udp_start(): 在 tcpip 线程里挂一个 lwIP raw UDP 回调（:5141）。 */

/* 处理一个 GET /pair。fd 是已 accept 的连接，请求行已确认是 GET /pair。
 * 内部自备响应缓冲（栈上，调用返回即释放）并直接发出。 */
void lanpair_http(int fd);

void lanpair_udp_start(void);
