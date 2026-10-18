/*
 * 照片播放（M3）：序列调度 + 给 stream.c 的帧快照。
 *
 * 调度任务只决定「当前该显示 board 还是哪张照片」，不做 IO；stream 的
 * 连接任务在帧边界取一次快照，照片字节由它自己从 SD 读并发出。
 *
 * 读卡只拿 photo_io 的 store 锁，不拿 net 锁，所以照片播放不会挡住取帧。
 */
#pragma once
#include <stdbool.h>
#include <stdint.h>

typedef struct {
    bool     active;
    uint8_t  sha256[32];
    uint32_t size;
    uint32_t start_sector;
} photo_play_frame_t;

/* app_main 在 framefetch_boot() 之后调用。 */
void photo_play_start(void);

/* 清单/索引变化时可选调用；正常靠 photo_sync_play_gen() 轮询。 */
void photo_play_notify_playlist(void);

/* 同一张照片连续读失败 3 次：标 bad、上报 status，不再重试。 */
void photo_play_mark_bad(const uint8_t sha[32], const char *why);

/* 一整张照片成功发完：清零连续失败计数。 */
void photo_play_frame_ok(const uint8_t sha[32]);

/* 序列当前项是照片且卡 READY（配对页期间为 false）。 */
bool photo_is_playing(void);

/* 维护窗口：让播放器在下一帧边界切回 board、等正在发的照片结束，
 * 网络/下载期间不播照片。返回 true 表示已让出；用完必须 resume。 */
bool photo_play_pause(uint32_t max_wait_ms);
void photo_play_resume(void);

/* 正在发送中的照片引用数（GC/格式化用）。 */
int photo_play_holds(void);

/* stream.c 帧边界调用：有可播照片则返回 true 并占一个 hold；
 * 调用方必须在发完/放弃后调 photo_play_frame_done()。 */
bool photo_play_frame(photo_play_frame_t *out);
void photo_play_frame_done(const photo_play_frame_t *pf);

/* GC 回调：该 sha 是否正被某个连接发送。调用方需持 store 锁。 */
bool photo_play_is_held(const uint8_t sha[32]);
