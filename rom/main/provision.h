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

/* 注册频率闸：1 小时内最多 3 次，超限返回 false（设备应停在配对页退避）。
 * 计数存 NVS，软件重启后仍有效。 */
bool prov_rereg_allowed(void);

/* 注册成功后调用一次，消耗当小时配额。 */
void prov_rereg_note(void);

/* 读自托管服务器地址（NVS namespace "cloud" / key "mqtt_uri"、"api_base"）。
 * 没配过的那一项填空串——调用方回落到 cloud_creds.h 的编译期默认值。
 * 两项独立，只改其中一个也成立。
 *
 * 地址必须能在运行期改，否则发不出一个所有人都能用的预编译固件，
 * 「浏览器一键烧录」就不成立。见 docs/PRODUCT.md 4.1。 */
void prov_load_server(char *mqtt_uri, size_t mcap, char *api_base, size_t acap);

/* 写自托管服务器地址。传空串或 NULL 表示删掉该项，回到默认值。 */
bool prov_save_server(const char *mqtt_uri, const char *api_base);

/* 清空已保存的凭据（开机时按住 MENU 键触发）。Wi-Fi 和设备密钥一起清。 */
void prov_erase(void);
