#pragma once
#include <stdbool.h>
#include <stdint.h>

/* 起云端长连接：没有设备密钥先注册出配对码，然后发心跳、订阅用量与配对通知。 */
void cloud_client_start(const uint8_t mac[6]);

/* 配对是否已完成。未配对时电视停在第 1 页显示配对码。 */
bool cloud_paired(void);

/* MQTT 长连接是否已建立。OTA 健康检查用它判断新固件能不能用。 */
bool cloud_connected(void);

/* 运行期生效的 API 根地址（不带尾斜杠），例如 https://host/api。
 * 值来自 NVS 的自托管设置，没配过则是 cloud_creds.h 的默认值。
 * cloud_client_start() 之后才有效。OTA 与 coredump 上传复用它。 */
const char *cloud_api_base(void);

/* 设备密钥，作 Bearer 用。未注册时是空串。 */
const char *cloud_devkey(void);

/* 电视上是不是正挂着配对码。挂着的时候屏幕归配对码，
 * 「连接中…」/「离线」状态页不要去覆盖它。 */
bool cloud_pair_pending(void);
