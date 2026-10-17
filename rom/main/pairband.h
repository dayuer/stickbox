#pragma once
#include "jband.h"

/* 配对码条带：电视第 1 页中间显示 6 位数字。
 * 只占 MCU 行 46..51（y 736..831），不重编码整帧。 */
#define PAIR_ROW0 46
#define PAIR_ROWS 6

/* 前景外接框，给 jb_encode_rows 跳过空白 MCU。 */
extern const jb_box_t PAIRBAND_BOX;

void pairband_show(const char *code);   /* 6 位数字；其他输入忽略 */
void pairband_clear(void);              /* 用背景色盖掉数字 */
