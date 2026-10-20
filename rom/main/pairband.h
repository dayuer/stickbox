#pragma once
#include "jband.h"

/* 默认条带：MCU 行 46..51（y 736..831，共 96 像素），状态页文字（statusband）
 * 居中在这里。stream_start 用它做默认行区间。 */
#define PAIR_ROW0 46
#define PAIR_ROWS 6

/* 配对页条带（#31）：从上到下是二维码、提示文字、6 位数字，MCU 行 27..50
 * （y 432..815）。比默认条带多出 18 行，每行即便全是底色也要约 485 字节，
 * 所以只有配对页发布这个区间（stream_band_publish_span），状态页仍走 46..51。 */
#define PAIRQR_ROW0 27
#define PAIRQR_ROWS 24

/* 前景外接框，给 jb_encode_rows 跳过空白 MCU。 */
extern const jb_box_t PAIRBAND_BOX;

/* code：6 位数字，其他输入忽略。url：二维码内容（NULL/空/过长则不画二维码）。 */
void pairband_show(const char *code, const char *url);
void pairband_clear(void);              /* 用背景色盖掉数字 */

/* 由 API 地址（形如 https://host/api，可带或不带末尾 "/"）和 12 位设备 ID
 * 拼出二维码内容 <站点根>/p?d=<devid>。站点根 = API 地址去掉末尾的 "/api"。
 * 放不进 cap、或参数为空时返回 false，buf 为空串。 */
bool pairband_url(char *buf, size_t cap, const char *api_base, const char *devid);
