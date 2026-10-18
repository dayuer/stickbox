#pragma once
#include <stdbool.h>

/* 取帧与换帧（T13b）。
 *
 * 服务端把渲染好的帧包（底帧 + 数字图集 + 索引）放在
 * GET {api_base}/device/frame，设备只负责存、拼时钟、推流。
 *
 * 时间线：
 *   app_main          framefetch_boot()   flash 里有有效帧就立刻起播
 *   云端连上          framefetch_start()  拉一次最新帧，之后每分钟换时钟带
 *   收到 frame 通知   framefetch_notify() 版本不同才重拉 */

/* 开机：挂上 flash 里已有的帧（有的话）并起时钟任务。
 * 必须在 stream_start() 之后调——要用它的带缓冲和页面解析结果。 */
void framefetch_boot(void);

/* 云端已连上、设备密钥已就位：拉第一帧。 */
void framefetch_start(void);

/* 收到 MQTT printer/{dev}/frame 通知。ver 与当前激活帧一样就什么都不做。 */
void framefetch_notify(const char *ver);

/* 立刻拉一次（幂等，已经在拉就直接返回）。why 只进日志。 */
void framefetch_request(const char *why);

/* 是否正有一轮取帧在跑（已请求、任务已建但还没结束）。
 * 照片下载拿它让路：取帧优先。 */
bool framefetch_busy(void);

/* 开机后第一轮取帧任务是否已经跑完（不论 200/304/失败）。
 * 照片开机任务用它避开和首个 TLS 握手叠加。 */
bool framefetch_first_done(void);
