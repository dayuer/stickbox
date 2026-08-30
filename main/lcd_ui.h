#pragma once
void lcd_ui_init(void);
/* 屏幕自上而下六行：
 *   1 StickBox（固定）  2 云端连接  3 本地 IP
 *   4 打印机名字        5 打印机状态 6 心跳
 * 作业和日志接在下面。 */
void lcd_ui_wifi(const char *t);       /* 第 2 行 */
void lcd_ui_ip(const char *t);         /* 第 3 行 */
void lcd_ui_prn_name(const char *t);   /* 第 4 行 */
void lcd_ui_prn(const char *t);
void lcd_ui_job(const char *t);
void lcd_ui_log(const char *line);
/* 心跳：只在 MQTT publish 真正成功时调，标题行会显示序号和距上次的秒数。
 * 超过 90 秒没有就显示「心跳停」并变红——云链路断了一眼可见。 */
void lcd_ui_beat(void);
