#pragma once
#include <stdbool.h>
#include <stdint.h>

/* 起云端长连接：没有设备密钥先注册出配对码，然后发心跳、订阅用量与配对通知。 */
void cloud_client_start(const uint8_t mac[6]);

/* 配对是否已完成。未配对时电视停在第 1 页显示配对码。 */
bool cloud_paired(void);
