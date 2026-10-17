#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

/* 帧包：服务端渲染的底帧 + 数字图集 + 索引（T13b）。
 *
 * 协议与字节规则见 docs/FRAME.md。本头文件分两层：
 *   - 下半部分的拼接核心是**纯 C、不依赖 ESP-IDF**，可以在主机上编译，
 *     拿 T13a 的黄金产物逐字节比对（见 rom/tools/bandtest）。
 *   - 上半部分是设备侧的存取与生命周期，依赖 IDF。
 */

/* ---- flash 槽布局 ----
 *
 * frames 分区 1.5MB 划 6 个 256KB 槽，环形轮换写（磨损均衡，见
 * rom/partitions.csv 的注释）。每槽：
 *
 *   [0:4]   "SBFS"      槽魔数
 *   [4:6]   ver (u16)   槽格式版本 = 1
 *   [6:8]   保留
 *   [8:12]  seq (u32)   单调递增，最大的那个有效槽是当前帧
 *   [12:16] pack_len    帧包字节数
 *   [16:20] crc32       帧包的 CRC32
 *   [20:32] 保留，擦除值 0xFF
 *   [32:]   帧包（T13c 的 SBF1 容器）
 *
 * 头部**最后写**：中途掉电时魔数写不全，这个槽自然失效，不会被当成
 * 一个完整的帧。
 *
 * 全部多字节字段用大端，和 T13c 的 SBF1 一致。 */
#define FP_SLOT_SIZE   (256 * 1024)
#define FP_SLOT_HEADER 32
#define FP_SLOT_COUNT  6
/* 一个帧包最大能有多少字节。服务端的构建上限必须 ≤ 这个数，否则会产出
 * 没有任何设备存得下的帧包。 */
#define FP_MAX_PACK    (FP_SLOT_SIZE - FP_SLOT_HEADER)

#define FP_SLOTS      4    /* HH:MM 四个数字槽 */
#define FP_BAND_ROWS 12    /* 时钟带占 12 个 MCU 行 */
#define FP_DIGITS    10

/* 一段连续字节。off 的基准见各字段说明。 */
typedef struct {
    uint32_t off;
    uint32_t len;
} fp_run_t;

/* 帧包索引：meta.json 解析后的结果。只留设备拼接真正用得到的字段。 */
typedef struct fp_index {
    /* 每个 MCU 行的时钟列区间（MCU 列 72..112），off 相对底帧起点。 */
    fp_run_t row_runs[FP_BAND_ROWS];
    /* 四个卡片槽在底帧里的位置，off 相对底帧起点。 */
    fp_run_t slot_runs[FP_SLOTS][FP_BAND_ROWS];
    /* 每个槽的 MCU 列，用来算 RSTn 序号。 */
    uint16_t slot_mcu_col[FP_SLOTS];
    /* 0–9 各数字的 12 段，off 相对 atlas 起点。 */
    fp_run_t digit_runs[FP_DIGITS][FP_BAND_ROWS];
    /* 时钟带起始 MCU 行（FRAME.md 里是 4）。算 RSTn 要用。 */
    uint16_t band_row0;
    /* 每个 MCU 行有多少个 restart 区间（1920/16/DRI = 15）。 */
    uint16_t intervals_per_row;
    /* 服务端遍历 1440 个时刻算出的最坏拼接长度。加载时必须校验它
     * 不超过 BAND_CAP——段长会随渲染器的字体和 q 值变化，光靠常量不够。 */
    uint32_t max_band_bytes;
} fp_index_t;

/* 按 HH:MM 拼出时钟带。
 *
 * base/atlas 可以是 flash 映射地址，函数只读不写它们。
 * out 至少要有 cap 字节；返回写入的字节数，0 表示失败（cap 不够或索引不合法）。
 *
 * 输出布局是 12 个行段首尾相接，每段对应 row_runs[i]——调用方按
 * row_runs 的绝对偏移把它们拼回推流里。
 *
 * hh/mm 超出范围会被拒绝：拼一张错的画面不如不拼。 */
size_t fp_build_band(const fp_index_t *ix,
                     const uint8_t *base, size_t base_len,
                     const uint8_t *atlas, size_t atlas_len,
                     int hh, int mm,
                     uint8_t *out, size_t cap);

/* 解析 meta.json 填 fp_index_t。失败返回 false——输入来自网络，一律当
 * 不可信处理。和拼接核心一样不依赖 IDF，主机侧测的就是这一份。 */
bool fp_parse_meta(const char *json, size_t len, fp_index_t *ix);

/* 索引自洽性检查：行段在底帧内、槽在行内且按列升序、数字段在图集内、
 * max_band_bytes 不超过本机缓冲。why 回填一句人能看懂的原因。
 *
 * 加载时查一次，比每帧拼接时才发现问题要早得多。 */
bool fp_index_valid(const fp_index_t *ix, size_t base_len, size_t atlas_len,
                    size_t band_cap, const char **why);

/* ---- 设备侧：帧包的存取（依赖 ESP-IDF）---- */

/* 找 frames 分区、扫槽、挑 seq 最大的有效槽并激活。
 * 分区不存在（旧分区表）或没有有效槽时返回 false——调用方退回预置页。 */
bool framepack_init(void);

/* 当前激活的帧包。没有激活帧时返回 false。
 * base/atlas 是 flash 映射地址，只读，生命周期跟着激活帧走。 */
bool framepack_active(const fp_index_t **ix,
                      const uint8_t **base, size_t *base_len,
                      const uint8_t **atlas, size_t *atlas_len);

/* 当前激活帧包的版本串（T13c 的 ver）。没有则返回空串。
 * 收到 frame 通知时拿它比对，一样就不用重新拉。 */
const char *framepack_version(void);

/* 作废所有帧槽并切回预置页。解绑/重新注册时调用：否则新主人会先用
 * flash 里上一个主人的帧起播。 */
void framepack_invalidate(void);

/* ---- 流式写入 ----
 *
 * 帧包约 218KB 而设备稳态空闲堆才 140KB，**装不下**。所以必须边收边写，
 * 不能先收进内存再落盘。 */
typedef struct {
    const void *part;      /* const esp_partition_t * */
    int      slot;         /* 正在写第几个槽 */
    uint32_t seq;          /* 这一帧的序号 */
    uint32_t pack_len;     /* 声明的帧包长度 */
    uint32_t written;      /* 已写入字节 */
    uint32_t crc;          /* 增量 CRC32 */
    bool     ok;           /* 中途出过错就一直是 false */
    char     ver[32];      /* 这一帧的版本串 */
} fp_writer_t;

/* 开始写一帧。pack_len 必须 ≤ FP_MAX_PACK；会擦掉目标槽。 */
bool fp_write_begin(fp_writer_t *w, uint32_t pack_len, const char *ver);

/* 写一段。返回 false 表示这一帧已经废了，调用方应当 abort。 */
bool fp_write_chunk(fp_writer_t *w, const void *data, size_t n);

/* 收尾：校验长度、解析并校验 meta、最后写槽头部并激活。
 * 任何一步不过就整帧丢弃，当前激活帧不受影响。 */
bool fp_write_finish(fp_writer_t *w);

/* 放弃这一帧。当前激活帧不受影响。 */
void fp_write_abort(fp_writer_t *w);

/* 拼接后各行段在输出缓冲里的偏移，和 fp_build_band 的写入顺序一致。
 * 推流时要用它把行段和底帧的连接段交替发出去。 */
void fp_band_layout(const fp_index_t *ix, uint32_t out_off[FP_BAND_ROWS],
                    uint32_t out_len[FP_BAND_ROWS], int hh, int mm);

/* 推流要发的一段连续字节。指针可能指向 flash 映射区或 RAM 缓冲。 */
typedef struct {
    const uint8_t *p;
    size_t         n;
} fp_seg_t;

/* 一帧最多 12×2+1 = 25 段。 */
#define FP_MAX_SEGS (FP_BAND_ROWS * 2 + 1)

/* 把底帧和拼好的时钟带编排成推流的段序列：
 * 底帧[0, 行0) 行0 底帧[行0后, 行1) 行1 … 行11 底帧[行11后, 结尾)
 *
 * 整帧从不在 RAM 里拼起来——底帧那些段直接指向 flash 映射地址。
 *
 * 返回段数，0 表示索引不合法。*total 回填这一帧的总字节数（AVI 块头要用）。
 * 这个编排和 stream.c 的发送循环共用，主机侧测试验的就是它。 */
int fp_frame_segments(const fp_index_t *ix,
                      const uint8_t *base, size_t base_len,
                      const uint8_t *band,
                      const uint32_t out_off[FP_BAND_ROWS],
                      const uint32_t out_len[FP_BAND_ROWS],
                      fp_seg_t *segs, int max_segs, size_t *total);
