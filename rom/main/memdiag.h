#pragma once

/* 内存对账（诊断用，Kconfig 默认关）。
 *
 * 为什么要有它：M0 上板实测稳态比 T8 少 55KB，我按代码推断过两次原因，
 * 两次都不对——第一次以为是 sdkconfig 被旧文件盖掉，查下来设置都生效；
 * 第二次以为是 pair_code_task 没退出，而日志显示设备当时已经配对。
 * 推断在这种问题上不可靠，只能把每个任务的栈和每个阶段的堆打出来对账。
 *
 * 关掉时所有函数编译成空，一个字节都不占。 */

/* 打一条堆快照：free / largest / 历史最低。label 标明是哪个阶段。 */
void memdiag_heap(const char *label);

/* 打全部任务的名字、栈底地址、栈余量最低值。
 * 需要 CONFIG_FREERTOS_USE_TRACE_FACILITY，诊断构建里一并打开。 */
void memdiag_tasks(const char *label);

/* 起一个定时器：开机 60 秒、120 秒各打一次完整快照（堆 + 任务）。 */
void memdiag_start(void);
