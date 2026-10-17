#pragma once
#include <stdint.h>
/* 起云端 MQTT 长连接：发心跳（判在线）并订阅用量主题。
 * 打印链路的 HTTPS 取件 / 作业 / 档案 / 探针命令都已随打印退役。 */
void cloud_client_start(const uint8_t mac[6]);
