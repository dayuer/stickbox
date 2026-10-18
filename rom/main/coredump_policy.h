#pragma once
/*
 * 崩溃镜像上传的退避策略（纯 C，不依赖 IDF，主机可测）。
 *
 * 为什么要有它：v1.1.0 发版前发现，服务端 nginx 把 40KB 的镜像按 32k 上限
 * 拒成 413，而固件的规则是「传不上就留着下次开机再传」——于是每台带镜像的
 * 设备每次开机都多一次 TLS + 40KB 上传，正好叠在照片首批帧上，把开机堆最低点
 * 打到 28–36K（门槛 40K）。生产 nginx 日志里一台板子就有 66 次 413。
 *
 * 服务端配置错一次，就变成「每次开机一次 TLS」，这是不能接受的放大。
 * 所以：同一份镜像每天最多试 CD_MAX_TRIES_PER_DAY 次，失败一律计数；
 * 成功才擦镜像（这条不变：传丢了还擦掉，现场就没了）。
 */
#include <stdbool.h>
#include <stdint.h>

#define CD_MAX_TRIES_PER_DAY 3
/* 时间还没同步（SNTP 未到）时的「今天」。这种开机不按日期重置计数，
 * 只受同一个上限约束，所以最坏是「未知日 3 次 + 每个已知日 3 次」。 */
#define CD_DAY_UNKNOWN 0u

typedef struct {
    uint32_t fp;     /* 镜像指纹：同一份镜像才累计；换了新镜像重新开始 */
    uint32_t day;    /* 最近一次计数所属的日期（Unix 天），CD_DAY_UNKNOWN=未知 */
    uint8_t  tries;  /* 该日已尝试次数 */
} cd_state_t;

typedef enum {
    CD_OK,          /* 2xx：服务端收下了，可以擦镜像 */
    CD_PERMANENT,   /* 4xx（除 408/429）：重试也不会好，例如 413/400/401/403 */
    CD_TRANSIENT,   /* 5xx / 408 / 429 / 网络错误（<=0）：下次可以再试 */
} cd_class_t;

cd_class_t cd_classify(int http_status);

/* 按 fp/today 归一化 st（新镜像或新的一天则清零计数），返回本次是否允许尝试。
 * st 会被就地修改，调用方应在随后写回持久化存储。 */
bool cd_should_try(cd_state_t *st, uint32_t fp, uint32_t today);

/* 记一次真正发出去的尝试（拿到了 HTTP 结果或网络错误）。
 * 只因堆不够/锁拿不到而没发出去的，不要调用——那不算一次尝试。 */
void cd_note_attempt(cd_state_t *st, int http_status);

/* 把 Unix 秒换成「天」；时间明显没同步（早于 2025-01-01）返回 CD_DAY_UNKNOWN。 */
uint32_t cd_day_of(int64_t unix_s);
