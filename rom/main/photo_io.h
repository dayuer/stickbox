/*
 * 网络 + SD 的互斥闸（M2）。
 *
 * 这些操作不能并发：
 *   - 取看板帧（TLS，约 40KB 堆）
 *   - 取清单 / 下照片（TLS + SD 写）
 *   - 以后照片推流时的 SD 读
 *
 * 规则只有一条：谁要用，谁先拿锁。取帧优先级高，所以下照片每次只拿
 * 一小段（4KB 读 + 写卡），并且每段之间检查 framefetch_busy()，有取帧
 * 就立刻放锁、保留 Range 偏移，等取帧完再续。
 *
 * 锁没建起来（内存不足）时退化为 no-op：宁可照片和取帧偶尔撞一下，
 * 也不能因为一把锁让看板再也不更新。
 */
#pragma once
#include <stdbool.h>
#include <stdint.h>

void photo_io_init(void);
bool photo_io_lock(uint32_t timeout_ms);
void photo_io_unlock(void);
