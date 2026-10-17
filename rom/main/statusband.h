#pragma once
#include <stdbool.h>

#include "statusmask.h"

/* 在纯色底图上显示一条功能性状态文字（「正在连接…」等）。
 *
 * 返回 false 表示没发布成功——调用方要知道这一点：画面会停在上一帧，
 * 而不是显示你以为的那条文字。 */
bool statusband_show(status_id_t id);
