/*
 * 配对码条带：七段数码管，纯矩形，不用字体。
 * 版面按 1080p / MCU 行 46..51（y 736..831）设计，6 位数字居中。
 */
#include "pairband.h"

#include <stdio.h>
#include <string.h>

#include "esp_log.h"
#include "qrcodegen.h"
#include "statusmask.h"
#include "stream.h"

static const char *TAG = "pairband";

/* 七段数字全部落在 8px 网格上：W=40、T=8、GAP=8，X0/Y0 都是 8 的倍数。
 * 为什么：编码器按 8x8 块做 DCT，整块同色时只剩 DC 系数，几乎不花字节；
 * 笔画边缘切在块中间的块要带一串 AC 系数。原来 36x56、笔画 7px 的版面里
 * 888888 加提示文字约 8.8KB（不含底色行）；对齐网格后只剩约 2.5KB。
 * 二维码同理：模块 8px 对齐块，约 2.2KB；6px 模块要 ~22KB。实测数据见 #31 的 PR。 */
#define X0   816
#define Y0   760
#define W     40
#define H     56
#define T      8
#define GAP    8
#define NDIG   6
/* 提示文字：与数字同屏，说明这串码用来干什么。 */
#define HINT_Y0 (PAIR_ROW0 * 16)
/* 必须和底图背景一致。原来是 0x1E3A8A（旧 page1 的蓝色背景），换成纯色
 * 底图之后忘了改，电视上就多了一道蓝带——这个现象实际报上来过。 */
#define BG   0x1D2D3D
#define FG   0xFFFFFF

/* 生成器里 "运行 stickbox-agent pair" 是 185×15；改文案后要
 * 同步这里（gen_statusmask.py 会打印新尺寸）。 */
#define HINT_W 185
#define HINT_X0 ((1920 - HINT_W) / 2)

/* 数字区横向范围：NDIG 位、位宽 W、间隔 GAP。 */
#define DIG_X1 (X0 + NDIG * W + (NDIG - 1) * GAP)
#define MIN2(a, b) ((a) < (b) ? (a) : (b))
#define MAX2(a, b) ((a) > (b) ? (a) : (b))
#define MIN3(a, b, c) MIN2(MIN2(a, b), c)
#define MAX3(a, b, c) MAX2(MAX2(a, b), c)

/* 二维码：URL 约 41 字符，字节模式 L 级正好 v3（29×29）。模块 6px，四周各 4 模块
 * 白色静区，整张白卡 (29+8)*6 = 222px 见方，画在提示文字上方，水平居中。
 * 深色模块用底色 BG，浅色/静区用 FG：jb_encode_rows 只有 bg/fg 两色，
 * 不必改编码器。URL 超出 v3 容量就不画（码和提示文字照常显示）。 */
#define QR_VER   3
#define QR_MOD   8
#define QR_QUIET 4
#define QR_CARD_MAX ((4 * QR_VER + 17 + 2 * QR_QUIET) * QR_MOD)   /* 222 */
#define QR_BOTTOM (HINT_Y0 - 8)
#define QR_TOP_MAX (QR_BOTTOM - QR_CARD_MAX)
#define QR_XMAX0 ((((1920 - QR_CARD_MAX) / 2) + 8) & ~15)

/* 横向必须覆盖「提示文字」「6 位数字」「二维码」的并集：只按提示文字算会得到
 * x∈[867,1052)，而数字占 [822,1098)，两端各一位被裁掉（#19）。
 * jb_encode_rows 按 MCU 与 box 求交，不要求 box 本身 16px 对齐。 */
const jb_box_t PAIRBAND_BOX = {
    MIN3(HINT_X0, X0, QR_XMAX0), QR_TOP_MAX,
    MAX3(HINT_X0 + HINT_W, DIG_X1, QR_XMAX0 + QR_CARD_MAX), Y0 + H
};

/*  段位：A 上 B 右上 C 右下 D 下 E 左下 F 左上 G 中 */
enum { A = 1, B = 2, C = 4, D = 8, E = 16, F = 32, G = 64 };
static const unsigned char SEG[10] = {
    A | B | C | D | E | F, B | C, A | B | D | E | G, A | B | C | D | G, B | C | F | G,
    A | C | D | F | G, A | C | D | E | F | G, A | B | C, 0x7F, A | B | C | D | F | G,
};

static bool in(int x, int y, int x0, int y0, int x1, int y1)
{
    return x >= x0 && x < x1 && y >= y0 && y < y1;
}

static bool digit(unsigned char s, int x, int y)
{
    const int m0 = H / 2 - T / 2, m1 = H / 2 + T / 2;
    return ((s & A) && in(x, y, T, 0, W - T, T)) ||
           ((s & B) && in(x, y, W - T, T, W, m0)) ||
           ((s & C) && in(x, y, W - T, m1, W, H - T)) ||
           ((s & D) && in(x, y, T, H - T, W - T, H)) ||
           ((s & E) && in(x, y, 0, m1, T, H - T)) ||
           ((s & F) && in(x, y, 0, T, T, m0)) ||
           ((s & G) && in(x, y, T, m0, W - T, m1));
}

/* 一次 show 内的着色上下文。qr 只在 pairband_show 的栈上活到编码结束，
 * 不常驻（qrcodegen_BUFFER_LEN_FOR_VERSION(3) = 107 B）。 */
typedef struct {
    const char *code;
    int qn;         /* 二维码边长（模块数）；0 = 不画 */
    int qx0, qy0;   /* 白卡左上角 */
    uint8_t qr[qrcodegen_BUFFER_LEN_FOR_VERSION(QR_VER)];
} pair_ctx_t;

/* 白卡（含静区）内：true = 浅色（FG），false = 深色模块（BG）。 */
static bool qr_shade(const pair_ctx_t *c, int x, int y)
{
    int mx = (x - c->qx0) / QR_MOD - QR_QUIET;
    int my = (y - c->qy0) / QR_MOD - QR_QUIET;
    if (mx < 0 || my < 0 || mx >= c->qn || my >= c->qn) return true;
    return !qrcodegen_getModule(c->qr, mx, my);
}

static bool pair_shade(int x, int y, void *ctx)
{
    const pair_ctx_t *c = ctx;
    if (c->qn) {
        int card = (c->qn + 2 * QR_QUIET) * QR_MOD;
        if (x >= c->qx0 && x < c->qx0 + card && y >= c->qy0 && y < c->qy0 + card)
            return qr_shade(c, x, y);
    }
    /* 提示文字：查生成好的 1-bit 掩码。 */
    const status_mask_t *hint = &STATUS_MASKS[STATUS_PAIR_HINT];
    int hx = x - HINT_X0, hy = y - HINT_Y0;
    if (hx >= 0 && hy >= 0 && hx < hint->w && hy < hint->h) {
        return (hint->bits[hy * hint->stride + (hx >> 3)] >> (7 - (hx & 7))) & 1;
    }

    /* 6 位七段数字。 */
    if (x < X0 || y < Y0 || y >= Y0 + H) return false;
    const char *code = c->code;
    int i = (x - X0) / (W + GAP);
    int lx = (x - X0) - i * (W + GAP);
    if (i < 0 || i >= NDIG || lx >= W) return false;
    char ch = code[i];
    if (ch < '0' || ch > '9') return false;
    return digit(SEG[ch - '0'], lx, y - Y0);
}

/* 把 url 编成二维码放进 c；不合适（过长/空）返回 false，c->qn 保持 0。 */
static bool qr_build(pair_ctx_t *c, const char *url)
{
    uint8_t tmp[qrcodegen_BUFFER_LEN_FOR_VERSION(QR_VER)];
    size_t len = url ? strlen(url) : 0;
    if (!len || len > sizeof tmp) return false;
    memcpy(tmp, url, len);
    if (!qrcodegen_encodeBinary(tmp, len, c->qr, qrcodegen_Ecc_LOW,
                                1, QR_VER, qrcodegen_Mask_AUTO, false))
        return false;
    c->qn = qrcodegen_getSize(c->qr);
    int card = (c->qn + 2 * QR_QUIET) * QR_MOD;
    c->qx0 = ((1920 - card) / 2 + 8) & ~15;   /* 对齐 16px（色度块）*/
    c->qy0 = QR_BOTTOM - card;
    return true;
}

/* 没有二维码时只编文字（提示 + 数字），走默认条带 46..51，装进常驻的 BAND_CAP。 */
static const jb_box_t PAIRTEXT_BOX = {
    MIN2(HINT_X0, X0), HINT_Y0,
    MAX2(HINT_X0 + HINT_W, DIG_X1), Y0 + H
};

static size_t encode_text(const char *code)
{
    pair_ctx_t c = { .code = code };
    uint8_t *buf = stream_band_begin();
    size_t n = jb_encode_rows(stream_page(0), PAIR_ROW0, PAIR_ROW0 + PAIR_ROWS,
                              BG, FG, pair_shade, &c, &PAIRTEXT_BOX, buf, stream_band_cap());
    if (!n) {
        ESP_LOGE(TAG, "配对码条带超出缓冲 %u B", (unsigned)stream_band_cap());
        return 0;
    }
    stream_band_publish(buf, n, 0);
    return n;
}

/* 带二维码的整条（27..50 行）编进临时大缓冲。大缓冲申请失败或二维码生成不了，
 * 都降级为 encode_text：码照常显示，只是没有二维码。 */
static size_t encode(const char *code, const char *url)
{
    pair_ctx_t c = { .code = code };
    if (!qr_build(&c, url)) {
        ESP_LOGW(TAG, "二维码未生成（URL 缺失或超出 v%d 容量），只显示配对码", QR_VER);
        size_t n = encode_text(code);
        stream_extra_release();   /* 不画二维码就不占大缓冲 */
        return n;
    }
    uint8_t *buf = stream_extra_acquire();
    if (!buf) return encode_text(code);
    size_t n = jb_encode_rows(stream_page(0), PAIRQR_ROW0, PAIRQR_ROW0 + PAIRQR_ROWS,
                              BG, FG, pair_shade, &c,
                              &PAIRBAND_BOX, buf, PAIR_BAND_CAP);
    if (!n) {
        ESP_LOGE(TAG, "配对码条带超出缓冲 %d B", PAIR_BAND_CAP);
        stream_extra_abort();
        return encode_text(code);
    }
    if (!stream_extra_publish(buf, n, 0, PAIRQR_ROW0, PAIRQR_ROWS)) return encode_text(code);
    return n;
}

bool pairband_url(char *buf, size_t cap, const char *api_base, const char *devid)
{
    if (!buf || !cap) return false;
    buf[0] = 0;
    if (!api_base || !devid || !api_base[0] || !devid[0]) return false;
    size_t n = strlen(api_base);
    while (n && api_base[n - 1] == '/') n--;
    if (n >= 4 && memcmp(api_base + n - 4, "/api", 4) == 0) n -= 4;
    if (!n) return false;
    int w = snprintf(buf, cap, "%.*s/p?d=%s", (int)n, api_base, devid);
    if (w < 0 || (size_t)w >= cap) { buf[0] = 0; return false; }
    return true;
}

void pairband_show(const char *code, const char *url)
{
    if (!code || strlen(code) != NDIG) return;
    size_t n = encode(code, url);
    if (n) ESP_LOGI(TAG, "配对码已上电视，条带 %u B", (unsigned)n);
}

void pairband_clear(void)
{
    /* 先还大缓冲（画面退回纯底图），再把默认条带编成背景色：二维码没画成时
     * 文字走的是默认条带，也要盖掉。 */
    stream_extra_release();
    static const jb_box_t empty = { 0, 0, 0, 0 };
    uint8_t *buf = stream_band_begin();
    size_t n = jb_encode_rows(stream_page(0), PAIR_ROW0, PAIR_ROW0 + PAIR_ROWS,
                              BG, BG, NULL, NULL, &empty, buf, stream_band_cap());
    if (!n) {
        ESP_LOGE(TAG, "清配对码条带失败（缓冲不够）");
        return;
    }
    stream_band_publish(buf, n, 0);
}
