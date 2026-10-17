/*
 * 帧包的 flash 存取（T13b）。
 *
 * 槽布局见 framepack.h。要点：
 *   - 帧包约 218KB，设备稳态空闲堆才 140KB——**装不下**，所以写入是流式的，
 *     边收边落盘；读取走 mmap，底帧和图集从不进 RAM。
 *   - 槽头部最后写。中途掉电时魔数写不全，那个槽自然失效，不会被当成
 *     一个完整的帧。
 *   - 六个槽环形轮换，为的是磨损均衡。寿命的账算在 partitions.csv 里：
 *     决定性因素是写入频率不是槽数，底帧只在数据变化时才写。
 */
#include <string.h>

#include "esp_crc.h"
#include "esp_log.h"
#include "esp_partition.h"

#include "framepack.h"
#include "stream.h"

static const char *TAG = "framepack";

#define SLOT_MAGIC "SBFS"
#define SLOT_VER   1
/* T13c 下发的帧包容器：magic + base_len + atlas_len + meta_len，全大端。 */
#define PACK_MAGIC "SBF1"
#define PACK_HEADER 16

static const esp_partition_t *s_part;

/* 当前激活帧。指针指向 mmap 出来的 flash 地址。 */
static esp_partition_mmap_handle_t s_map;
static bool           s_mapped;
static const uint8_t *s_base;
static size_t         s_base_len;
static const uint8_t *s_atlas;
static size_t         s_atlas_len;
static fp_index_t     s_index;
static bool           s_active;
static uint32_t       s_seq;
static int            s_slot = -1;
static char           s_ver[32];

static uint32_t be32(const uint8_t *p)
{
    return ((uint32_t)p[0] << 24) | ((uint32_t)p[1] << 16) |
           ((uint32_t)p[2] << 8) | p[3];
}

static void put_be32(uint8_t *p, uint32_t v)
{
    p[0] = (uint8_t)(v >> 24); p[1] = (uint8_t)(v >> 16);
    p[2] = (uint8_t)(v >> 8);  p[3] = (uint8_t)v;
}

/* ------------------------------------------------------------ 槽的读取 */

/* 读槽头部。返回 false 表示这个槽不是一个完整的帧。 */
static bool read_slot_header(int slot, uint32_t *seq, uint32_t *len,
                             uint32_t *crc, char *ver, size_t vercap)
{
    uint8_t h[FP_SLOT_HEADER];
    size_t off = (size_t)slot * FP_SLOT_SIZE;
    if (esp_partition_read(s_part, off, h, sizeof h) != ESP_OK) return false;
    if (memcmp(h, SLOT_MAGIC, 4) != 0) return false;
    if (((uint32_t)h[4] << 8 | h[5]) != SLOT_VER) return false;
    *seq = be32(h + 8);
    *len = be32(h + 12);
    *crc = be32(h + 16);
    if (*len == 0 || *len > FP_MAX_PACK) return false;
    /* 版本串放在保留区的前 12 字节。旧写入方只拷实际长度，剩余是 0xFF
     * 擦除态——必须按定长字段解码并补 0，不能直接当 C 字符串。 */
    fp_slot_ver(ver, vercap, h + 20);
    return true;
}

/* 校验槽里帧包的 CRC。分块读，不占堆。 */
static bool slot_crc_ok(int slot, uint32_t len, uint32_t want)
{
    static uint8_t buf[1024];
    size_t base = (size_t)slot * FP_SLOT_SIZE + FP_SLOT_HEADER;
    uint32_t crc = 0;
    for (uint32_t off = 0; off < len; ) {
        size_t n = len - off < sizeof buf ? len - off : sizeof buf;
        if (esp_partition_read(s_part, base + off, buf, n) != ESP_OK) return false;
        crc = esp_crc32_le(crc, buf, n);
        off += (uint32_t)n;
    }
    return crc == want;
}

/* 把槽里的帧包映射进来并解析。成功后 s_* 指向新帧。 */
static bool activate(int slot, uint32_t len, const char *ver)
{
    const void *p = NULL;
    esp_partition_mmap_handle_t h;
    size_t off = (size_t)slot * FP_SLOT_SIZE + FP_SLOT_HEADER;
    if (esp_partition_mmap(s_part, off, len, ESP_PARTITION_MMAP_DATA, &p, &h) != ESP_OK) {
        ESP_LOGE(TAG, "槽 %d 映射失败", slot);
        return false;
    }
    const uint8_t *pk = p;

    bool ok = len > PACK_HEADER && memcmp(pk, PACK_MAGIC, 4) == 0;
    uint32_t bl = 0, al = 0, ml = 0;
    if (ok) {
        bl = be32(pk + 4); al = be32(pk + 8); ml = be32(pk + 12);
        /* 三段长度必须正好拼满，多一字节少一字节都说明这不是我们认识的包。 */
        ok = (uint64_t)PACK_HEADER + bl + al + ml == len && bl && al && ml;
    }
    fp_index_t ix;
    if (ok) ok = fp_parse_meta((const char *)(pk + PACK_HEADER + bl + al), ml, &ix);
    const char *why = "";
    if (ok) ok = fp_index_valid(&ix, bl, al, stream_band_cap(), &why);

    if (!ok) {
        ESP_LOGE(TAG, "槽 %d 的帧包不可用：%s", slot, why[0] ? why : "容器或 meta 不合法");
        esp_partition_munmap(h);
        return false;
    }

    /* 新帧一切就绪，才换掉旧的——中途失败时画面继续用上一帧。
     *
     * **释放旧映射之前必须先让推流松手。** 发送循环在每帧开头把 s_base
     * 快照进段表，之后一路 send_all 不再回头看共享状态；这段时间里旧的
     * flash 映射必须一直有效。这里直接 munmap 就是把地址从正在发送的段
     * 底下抽走，电视上先是花屏，严重时取到已经被别的东西占用的 flash。
     *
     * stream_use_framepack() 内部会等在途的帧发完（fp_wait_idle），所以
     * 顺序必须是「先切 stream 再 munmap」。 */
    stream_use_framepack(&ix, pk + PACK_HEADER, bl);
    if (s_mapped) esp_partition_munmap(s_map);
    s_map = h; s_mapped = true;
    s_base = pk + PACK_HEADER;      s_base_len = bl;
    s_atlas = s_base + bl;          s_atlas_len = al;
    s_index = ix;
    s_slot = slot;
    s_active = true;
    strlcpy(s_ver, ver ? ver : "", sizeof s_ver);
    ESP_LOGI(TAG, "激活槽 %d：底帧 %u、图集 %u、meta %u，最坏带长 %u，版本 %s",
             slot, (unsigned)bl, (unsigned)al, (unsigned)ml,
             (unsigned)ix.max_band_bytes, s_ver);
    return true;
}

/* 找分区但不扫槽/不激活。解绑清理和开机无密钥清理都要先有 s_part。 */
static bool ensure_part(void)
{
    if (s_part) return true;
    s_part = esp_partition_find_first(ESP_PARTITION_TYPE_DATA, 0x40, "frames");
    if (!s_part) {
        /* 旧分区表（T9 之前）没有这个分区。不是致命错误：退回预置页，
         * 设备照常配网、配对、推流，只是没有看板。 */
        ESP_LOGW(TAG, "没有 frames 分区，退回预置页");
        return false;
    }
    if (s_part->size < (size_t)FP_SLOT_COUNT * FP_SLOT_SIZE) {
        ESP_LOGE(TAG, "frames 分区只有 %u 字节，装不下 %d 个槽",
                 (unsigned)s_part->size, FP_SLOT_COUNT);
        s_part = NULL;
        return false;
    }
    return true;
}

bool framepack_init(void)
{
    if (!ensure_part()) return false;

    int best = -1;
    uint32_t best_seq = 0, best_len = 0;
    char best_ver[32] = { 0 };
    for (int i = 0; i < FP_SLOT_COUNT; i++) {
        uint32_t seq, len, crc;
        char ver[32];
        if (!read_slot_header(i, &seq, &len, &crc, ver, sizeof ver)) continue;
        if (best >= 0 && seq <= best_seq) continue;
        if (!slot_crc_ok(i, len, crc)) {
            ESP_LOGW(TAG, "槽 %d 头部完整但 CRC 不符，跳过", i);
            continue;
        }
        best = i; best_seq = seq; best_len = len;
        strlcpy(best_ver, ver, sizeof best_ver);
    }
    if (best < 0) {
        ESP_LOGI(TAG, "没有有效帧包，等服务端下发");
        return false;
    }
    s_seq = best_seq;
    return activate(best, best_len, best_ver);
}

bool framepack_active(const fp_index_t **ix,
                      const uint8_t **base, size_t *base_len,
                      const uint8_t **atlas, size_t *atlas_len)
{
    if (!s_active) return false;
    if (ix) *ix = &s_index;
    if (base) *base = s_base;
    if (base_len) *base_len = s_base_len;
    if (atlas) *atlas = s_atlas;
    if (atlas_len) *atlas_len = s_atlas_len;
    return true;
}

const char *framepack_version(void) { return s_active ? s_ver : ""; }

static void erase_all_slots(void)
{
    /* 只擦每个槽的第一个扇区（含 magic+header），整槽擦除没必要还费寿命。 */
    for (int i = 0; i < FP_SLOT_COUNT; i++) {
        const size_t off = (size_t)i * FP_SLOT_SIZE;
        esp_err_t err = esp_partition_erase_range(s_part, off, 4096);
        if (err != ESP_OK) {
            ESP_LOGE(TAG, "作废槽 %d 失败：%s", i, esp_err_to_name(err));
        }
    }
    s_active = false;
    s_seq = 0;
    s_ver[0] = 0;
}

void framepack_invalidate(void)
{
    if (!ensure_part()) {
        s_active = false;
        s_ver[0] = 0;
        return;
    }
    /* 先让推流松手，再 munmap，最后擦槽头。
     * 擦除期间 flash 映射不可用，顺序反了推流任务会读到失效地址。 */
    stream_use_presets();
    if (s_mapped) {
        esp_partition_munmap(s_map);
        s_mapped = false;
    }
    erase_all_slots();
    ESP_LOGI(TAG, "frames 槽已作废，切回预置页");
}

void framepack_invalidate_reboot(void)
{
    /* 解绑清理路径：马上要 esp_restart，不能等推流 idle（电视卡住时
     * fp_wait_idle 可能等很久，设备就一直不重启）。只把“下一帧用预置页”
     * 切过去，不 munmap——在途发送可能还拿着映射，重启后自然释放。
     * 若擦除与在途读取撞车导致 panic，设备照样重启；开机时没有密钥会
     * 再次擦槽（见 framefetch_boot 的守卫），隐私不会漏。 */
    if (!ensure_part()) {
        ESP_LOGW(TAG, "cleanup: 没有 frames 分区");
        return;
    }
    stream_use_presets_nowait();
    ESP_LOGW(TAG, "cleanup: 作废 frames 槽");
    erase_all_slots();
}

/* ------------------------------------------------------------ 流式写入 */

bool fp_write_begin(fp_writer_t *w, uint32_t pack_len, const char *ver)
{
    memset(w, 0, sizeof *w);
    if (!s_part) return false;
    if (pack_len == 0 || pack_len > FP_MAX_PACK) {
        ESP_LOGE(TAG, "帧包 %u 字节超过槽容量 %u，拒收",
                 (unsigned)pack_len, (unsigned)FP_MAX_PACK);
        return false;
    }
    /* 写下一个槽，不覆盖当前激活的那个——新帧落盘失败时还得靠它出画面。 */
    int slot = (s_slot + 1) % FP_SLOT_COUNT;
    size_t off = (size_t)slot * FP_SLOT_SIZE;
    if (esp_partition_erase_range(s_part, off, FP_SLOT_SIZE) != ESP_OK) {
        ESP_LOGE(TAG, "擦槽 %d 失败", slot);
        return false;
    }
    w->part = s_part;
    w->slot = slot;
    w->seq = s_seq + 1;
    w->pack_len = pack_len;
    w->ok = true;
    strlcpy(w->ver, ver ? ver : "", sizeof w->ver);
    return true;
}

bool fp_write_chunk(fp_writer_t *w, const void *data, size_t n)
{
    if (!w->ok) return false;
    if (w->written + n > w->pack_len) {
        /* 服务端给的比声明的多。宁可整帧丢弃，也不能写出槽外。 */
        ESP_LOGE(TAG, "帧包超出声明长度 %u，丢弃", (unsigned)w->pack_len);
        w->ok = false;
        return false;
    }
    size_t off = (size_t)w->slot * FP_SLOT_SIZE + FP_SLOT_HEADER + w->written;
    if (esp_partition_write(w->part, off, data, n) != ESP_OK) {
        ESP_LOGE(TAG, "写槽 %d 失败", w->slot);
        w->ok = false;
        return false;
    }
    w->crc = esp_crc32_le(w->crc, data, n);
    w->written += (uint32_t)n;
    return true;
}

bool fp_write_finish(fp_writer_t *w)
{
    if (!w->ok) return false;
    if (w->written != w->pack_len) {
        ESP_LOGE(TAG, "帧包只收到 %u/%u 字节，丢弃",
                 (unsigned)w->written, (unsigned)w->pack_len);
        return false;
    }

    /* 先把这个槽当成候选解析一遍：meta 不合法、max_band_bytes 超过本机
     * 缓冲的包，绝不能被写上头部变成「有效帧」——否则下次开机会优先选中
     * 它，而它根本用不了。 */
    uint8_t h[FP_SLOT_HEADER];
    memset(h, 0xFF, sizeof h);
    memcpy(h, SLOT_MAGIC, 4);
    h[4] = 0; h[5] = SLOT_VER;
    h[6] = h[7] = 0;
    put_be32(h + 8, w->seq);
    put_be32(h + 12, w->pack_len);
    put_be32(h + 16, w->crc);
    /* 版本字段是定长 12 字节，必须用 0 补满；0xFF 填充会让旧读取方
     * 把擦除态当成版本号的一部分。 */
    memset(h + 20, 0, 12);
    memcpy(h + 20, w->ver, strnlen(w->ver, 12));

    /* 头部最后写：掉电时魔数写不全，这个槽自然失效。 */
    size_t off = (size_t)w->slot * FP_SLOT_SIZE;
    if (esp_partition_write(w->part, off, h, sizeof h) != ESP_OK) {
        ESP_LOGE(TAG, "写槽 %d 头部失败", w->slot);
        return false;
    }
    if (!activate(w->slot, w->pack_len, w->ver)) {
        /* 激活失败说明这个包有问题。头部已经写了，但 activate 里已经报过
         * 原因；下次开机会再选中它、再失败一次，然后退回预置页。
         * 比起静默接受一个用不了的帧，这样至少日志里有迹可循。 */
        return false;
    }
    s_seq = w->seq;
    w->ok = false;   /* 防止重复 finish */
    return true;
}

void fp_write_abort(fp_writer_t *w)
{
    if (w->ok) ESP_LOGW(TAG, "放弃槽 %d 上这一帧", w->slot);
    w->ok = false;
}
