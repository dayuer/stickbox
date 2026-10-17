#include "clockface.h"

#define X0 80
#define Y0 816   /* 带内上下各留 16px */
#define H  128
#define W  72
#define T  14
#define GAP 24
#define COLON_W 36

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

const jb_box_t CLOCKFACE_BOX = { X0, Y0, X0 + 4 * W + COLON_W + 4 * GAP, Y0 + H };

bool clockface_shade(int x, int y, void *ctx)
{
    if (y < Y0 || y >= Y0 + H || x < X0) return false;
    const clockface_t *c = ctx;
    int cx = X0;
    y -= Y0;
    for (int i = 0; c->txt[i]; i++) {
        char ch = c->txt[i];
        int cw = ch == ':' ? COLON_W : W;
        if (x < cx + cw) {
            int lx = x - cx;
            if (ch == ':')
                return c->colon_on && lx >= (COLON_W - T) / 2 && lx < (COLON_W + T) / 2 &&
                       ((y >= H / 3 - T / 2 && y < H / 3 + T / 2) ||
                        (y >= 2 * H / 3 - T / 2 && y < 2 * H / 3 + T / 2));
            if (ch == '-') return digit(G, lx, y);
            if (ch >= '0' && ch <= '9') return digit(SEG[ch - '0'], lx, y);
            return false;
        }
        cx += cw + GAP;
        if (x < cx) return false;  /* 字间空隙 */
    }
    return false;
}
