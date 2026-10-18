/*
 * 网络 / SD 的两把互斥闸（M3 拆开）。
 *
 * 不能并发的组合和原因：
 *   - 取帧 TLS ↔ 清单/下载 TLS：两个 TLS 会话同时开要 ~80KB 堆，稳态不够。
 *   - 下载写卡 ↔ 照片播放读卡：photostore 共用一块 4KB 缓冲，SD 事务也
 *     不适合交错到字节级。
 *
 * 但「照片播放」和「取帧」并不共享资源：播放只读 SD，取帧只走网络。
 * 所以 M3 把它们分到两把锁：
 *
 *   net   取帧、清单拉取、下载的 HTTP 段
 *   store 下载写卡、GC/元数据/格式化、照片播放读卡
 *
 * **锁序固定 net → store，禁止反向**（下载在持 net 时按需拿 store）。
 * 播放只拿 store，因此永远不会挡取帧；取帧最坏只被清单/下载的 HTTP
 * 段挡住，量级见 docs/PHOTO-M3-P0.md。
 *
 * 锁没建起来（内存不足）时退化为 no-op：宁可偶发并发，也不能因为一把锁
 * 让看板再也不更新。
 */
#pragma once
#include <stdbool.h>
#include <stdint.h>

void photo_io_init(void);

bool photo_io_net_lock(uint32_t timeout_ms);
void photo_io_net_unlock(void);

bool photo_io_store_lock(uint32_t timeout_ms);
void photo_io_store_unlock(void);
