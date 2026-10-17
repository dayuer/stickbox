#include "jband.h"

#include <math.h>
#include <string.h>

#include "esp_log.h"

static const char *TAG = "jband";

static const uint8_t ZZ[64] = {  /* zigzag 序号 → 自然序位置 */
    0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5,
    12, 19, 26, 33, 40, 48, 41, 34, 27, 20, 13, 6, 7, 14, 21, 28,
    35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51,
    58, 59, 52, 45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63,
};

/* 编码器状态：从第一张页面的 DQT / DHT 里取 */
static bool s_tables;
static float s_qinv[2][64];          /* 自然序，已并入 DCT 归一化 */
static uint16_t s_code[4][256];      /* 0=DC 亮度 1=AC 亮度 2=DC 色度 3=AC 色度 */
static uint8_t s_size[4][256];
static float s_cos[8][8];

/* ── 解析 ── */
static void build_huff(int slot, const uint8_t *bits, const uint8_t *vals)
{
    uint16_t code = 0;
    int k = 0;
    for (int len = 1; len <= 16; len++) {
        for (int i = 0; i < bits[len - 1]; i++) {
            s_code[slot][vals[k]] = code++;
            s_size[slot][vals[k]] = len;
            k++;
        }
        code <<= 1;
    }
}

bool jb_parse(jb_page_t *p, const uint8_t *d, size_t n)
{
    memset(p, 0, sizeof *p);
    p->jpg = d; p->len = n;
    if (n < 4 || d[0] != 0xFF || d[1] != 0xD8) return false;
    bool first = !s_tables;
    int dri = 0;
    size_t i = 2, scan = 0;
    while (i + 4 <= n && !scan) {
        if (d[i] != 0xFF) return false;
        uint8_t m = d[i + 1];
        size_t L = (d[i + 2] << 8) | d[i + 3];
        const uint8_t *s = d + i + 4;
        if (m == 0xC0) {
            p->h = (s[1] << 8) | s[2];
            p->w = (s[3] << 8) | s[4];
            if (s[5] != 3 || s[7] != 0x22 || s[10] != 0x11 || s[13] != 0x11) {
                ESP_LOGE(TAG, "只支持 YCbCr 4:2:0");
                return false;
            }
        } else if (m == 0xC2 || m == 0xC1) {
            ESP_LOGE(TAG, "只支持 baseline");
            return false;
        } else if (m == 0xDD) {
            dri = (s[0] << 8) | s[1];
        } else if (m == 0xDB && first) {
            for (size_t k = 0; k + 65 <= L - 2; k += 65) {
                int id = s[k] & 0x0F;
                if (id > 1 || (s[k] >> 4)) return false;
                for (int z = 0; z < 64; z++) {
                    int nat = ZZ[z], u = nat & 7, v = nat >> 3;
                    float cu = u ? 1.f : (float)M_SQRT1_2, cv = v ? 1.f : (float)M_SQRT1_2;
                    /* F(u,v) = 1/4 C(u)C(v) ΣΣ …；把 1/4·C·C 和 1/q 合成一个乘数 */
                    s_qinv[id][nat] = 0.25f * cu * cv / s[k + 1 + z];
                }
            }
        } else if (m == 0xC4 && first) {
            size_t k = 0;
            while (k < L - 2) {
                int tc = s[k] >> 4, th = s[k] & 0x0F;
                const uint8_t *bits = s + k + 1;
                int cnt = 0;
                for (int b = 0; b < 16; b++) cnt += bits[b];
                build_huff(th * 2 + tc, bits, bits + 16);
                k += 17 + cnt;
            }
        } else if (m == 0xDA) {
            scan = i + 2 + L;
        }
        i += 2 + L;
    }
    p->mcu_cols = (p->w + 15) / 16;
    p->mcu_rows = (p->h + 15) / 16;
    if (!scan || dri != p->mcu_cols || p->mcu_rows > JB_MAX_ROWS) {
        ESP_LOGE(TAG, "需要 DRI = 每行 MCU 数（%d），实际 %d", p->mcu_cols, dri);
        return false;
    }

    /* 扫熵编码区找 RSTn：FF 00 是填充，FF D0..D7 是段边界 */
    int r = 0;
    p->row_off[0] = scan;
    for (size_t k = scan; k + 1 < n; k++) {
        if (d[k] != 0xFF) continue;
        uint8_t m = d[k + 1];
        if (m >= 0xD0 && m <= 0xD7) {
            p->row_off[++r] = k + 2;
            k++;
        } else if (m == 0xD9) {
            p->row_off[p->mcu_rows] = k;
            break;
        } else {
            k++;
        }
    }
    if (r != p->mcu_rows - 1) {
        ESP_LOGE(TAG, "RST 数 %d，期望 %d", r, p->mcu_rows - 1);
        return false;
    }
    if (first) {
        for (int u = 0; u < 8; u++)
            for (int x = 0; x < 8; x++) s_cos[u][x] = cosf((2 * x + 1) * u * (float)M_PI / 16);
        s_tables = true;
    }
    return true;
}

/* ── 位写入 ── */
typedef struct {
    uint8_t *buf;
    size_t cap, n;
    uint32_t acc;
    int bits;
    bool full;
} bw_t;

static inline void emit(bw_t *w, uint8_t b)
{
    if (w->n + 2 > w->cap) { w->full = true; return; }
    w->buf[w->n++] = b;
    if (b == 0xFF) w->buf[w->n++] = 0;
}

static inline void put(bw_t *w, uint32_t code, int size)
{
    w->acc = (w->acc << size) | (code & ((1u << size) - 1));
    w->bits += size;
    while (w->bits >= 8) {
        w->bits -= 8;
        emit(w, (w->acc >> w->bits) & 0xFF);
    }
}

static void pad(bw_t *w)
{
    if (w->bits) put(w, 0x7F, 8 - w->bits);  /* 用 1 补齐字节 */
}

static inline int nbits(int v)
{
    v = v < 0 ? -v : v;
    int n = 0;
    while (v) { n++; v >>= 1; }
    return n;
}

static void put_val(bw_t *w, int slot, int sym, int v, int sz)
{
    put(w, s_code[slot][sym], s_size[slot][sym]);
    if (sz) put(w, v < 0 ? v - 1 : v, sz);  /* 负数用反码的低位 */
}

/* 一个 8x8 块：samples 已减 128。uniform = 全块同值时走快速路径（只有 DC） */
static void enc_block(bw_t *w, const float *s, bool uniform, int comp, int *pred)
{
    const int dc_slot = comp ? 2 : 0, ac_slot = comp ? 3 : 1;
    const float *qi = s_qinv[comp ? 1 : 0];
    int q[64];
    if (uniform) {
        memset(q, 0, sizeof q);
        q[0] = (int)lrintf(s[0] * 64.f * qi[0]);  /* ΣΣ 64 个同值，qi 里已含 ¼·C(0)² */
    } else {
        float t[64];
        for (int y = 0; y < 8; y++)
            for (int u = 0; u < 8; u++) {
                float a = 0;
                for (int x = 0; x < 8; x++) a += s[y * 8 + x] * s_cos[u][x];
                t[y * 8 + u] = a;
            }
        for (int v = 0; v < 8; v++)
            for (int u = 0; u < 8; u++) {
                float a = 0;
                for (int y = 0; y < 8; y++) a += t[y * 8 + u] * s_cos[v][y];
                q[v * 8 + u] = (int)lrintf(a * qi[v * 8 + u]);
            }
    }
    int diff = q[0] - *pred;
    *pred = q[0];
    int sz = nbits(diff);
    put_val(w, dc_slot, sz, diff, sz);

    int run = 0;
    for (int z = 1; z < 64; z++) {
        int c = q[ZZ[z]];
        if (!c) { run++; continue; }
        while (run > 15) { put(w, s_code[ac_slot][0xF0], s_size[ac_slot][0xF0]); run -= 16; }
        sz = nbits(c);
        put_val(w, ac_slot, (run << 4) | sz, c, sz);
        run = 0;
    }
    if (run) put(w, s_code[ac_slot][0x00], s_size[ac_slot][0x00]);  /* EOB */
}

/* ── 编码若干 MCU 行 ── */
static void ycc(uint32_t rgb, float *y, float *cb, float *cr)
{
    float r = (rgb >> 16) & 0xFF, g = (rgb >> 8) & 0xFF, b = rgb & 0xFF;
    *y = 0.299f * r + 0.587f * g + 0.114f * b - 128;
    *cb = -0.168736f * r - 0.331264f * g + 0.5f * b;
    *cr = 0.5f * r - 0.418688f * g - 0.081312f * b;
}

size_t jb_encode_rows(const jb_page_t *p, int r0, int r1, uint32_t bg, uint32_t fg,
                      jb_shader_t shade, void *ctx, const jb_box_t *box, uint8_t *out, size_t cap)
{
    bw_t w = { .buf = out, .cap = cap };
    float yb, cbb, crb, yf, cbf, crf;
    ycc(bg, &yb, &cbb, &crb);
    ycc(fg, &yf, &cbf, &crf);

    static float Y[4][64], Cb[64], Cr[64];
    static uint8_t m[16][16];  /* 本 MCU 的前景掩码 */
    for (int r = r0; r < r1; r++) {
        int pred[3] = { 0, 0, 0 };  /* restart 段起点：DC 预测清零 */
        for (int c = 0; c < p->mcu_cols; c++) {
            int x0 = c * 16, y0 = r * 16, nfg = 0;
            /* 框外的 MCU 不调着色器——ESP32 上逐像素调用是大头（实测整带 459ms） */
            if (x0 + 16 > box->x0 && x0 < box->x1 && y0 + 16 > box->y0 && y0 < box->y1)
                for (int dy = 0; dy < 16; dy++)
                    for (int dx = 0; dx < 16; dx++)
                        nfg += m[dy][dx] = shade(x0 + dx, y0 + dy, ctx);

            const bool flat = nfg == 0 || nfg == 256;
            const float yv = nfg ? yf : yb;
            for (int b = 0; b < 4; b++) {
                if (flat) {
                    Y[b][0] = yv;
                } else {
                    int bx = (b & 1) * 8, by = (b >> 1) * 8;
                    for (int k = 0; k < 64; k++)
                        Y[b][k] = m[by + (k >> 3)][bx + (k & 7)] ? yf : yb;
                }
            }
            if (flat) {
                Cb[0] = nfg ? cbf : cbb;
                Cr[0] = nfg ? crf : crb;
            } else {
                for (int k = 0; k < 64; k++) {  /* 2x2 平均下采样 */
                    int sy = (k >> 3) * 2, sx = (k & 7) * 2;
                    int f = m[sy][sx] + m[sy][sx + 1] + m[sy + 1][sx] + m[sy + 1][sx + 1];
                    Cb[k] = (f * cbf + (4 - f) * cbb) / 4;
                    Cr[k] = (f * crf + (4 - f) * crb) / 4;
                }
            }
            for (int b = 0; b < 4; b++) enc_block(&w, Y[b], flat, 0, &pred[0]);
            enc_block(&w, Cb, flat, 1, &pred[1]);
            enc_block(&w, Cr, flat, 2, &pred[2]);
        }
        pad(&w);
        if (r < p->mcu_rows - 1) {  /* 段尾 RSTn，编号 = 行号 mod 8 */
            if (w.n + 2 > w.cap) w.full = true;
            else { w.buf[w.n++] = 0xFF; w.buf[w.n++] = 0xD0 + (r & 7); }
        }
        if (w.full) return 0;
    }
    return w.n;
}
