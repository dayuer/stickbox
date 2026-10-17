#pragma once
#include <stdbool.h>
#include <stddef.h>

/* 从 NVS 读取已保存的 Wi-Fi 凭据，无则返回 false */
bool prov_load(char *ssid, size_t ssid_len, char *pass, size_t pass_len);

/* 起配网门户（开放热点 + DNS 劫持 + 网页），阻塞直到用户配好并验证通过。
 * 返回后凭据已写入 NVS。 */
void prov_portal_run(void);

/* 读设备密钥（NVS namespace "cloud" / key "devkey"）。
 * 格式是 {key_id}.{secret}，45 字符。**按不透明字符串处理，不解析不截断。**
 * 没配过返回 false —— 此时设备先走 /api/device/register 出配对码。 */
bool prov_load_devkey(char *out, size_t cap);

/* 把服务端注册返回的设备密钥写进 NVS。 */
bool prov_save_devkey(const char *key);

/* 只清设备密钥，保留 Wi-Fi。密钥被吊销/解绑后回到注册流程时用。 */
void prov_erase_devkey(void);

/* 清空已保存的凭据（开机时按住 MENU 键触发）。Wi-Fi 和设备密钥一起清。 */
void prov_erase(void);
