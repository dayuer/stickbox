#!/usr/bin/env python3
"""把固定的几条状态文字预渲染成 1-bit 掩码，生成 main/statusmask.c。

为什么在构建期做，而不是在设备上画字：
  - 设备上唯一的中文字体是 LVGL 格式的 font_cn_16，LVGL 9 取字模要先搭一个
    lv_draw_buf_t。把推流路径耦合到 LVGL 不划算，还得在 1080p 上把 16px
    放大四倍，边缘会很难看。
  - 这些文字是固定的几条，不需要任何运行期排版能力。预渲染之后设备侧
    只剩一次查位，零字体逻辑。

掩码是 1-bit、按行打包，行宽向上取整到字节。设备侧的着色器直接按
(x, y) 查位，不做缩放——所见即所得。

改了文案就重跑一次这个脚本，把生成的 statusmask.c 一起提交。
"""
import sys
from PIL import Image, ImageDraw, ImageFont

FONT = "/System/Library/Fonts/STHeiti Medium.ttc"
SIZE = 64          # 1080p 上够看清，且掩码不至于太大
BAND_H = 96        # MCU 行 46..51 = 96 像素

# 名字 → 文案。名字会变成 C 里的枚举值。
TEXTS = [
    ("CONNECTING", "正在连接…"),
    ("FETCHING",   "正在获取数据"),
    ("OFFLINE",    "网络已断开"),
    ("WAIT_PAIR",  "等待配对"),
    ("NO_SERVER",  "连不上服务器"),
]


def render(text):
    font = ImageFont.truetype(FONT, SIZE)
    # 先量尺寸，再按实际大小画，省得掩码里全是空白。
    probe = ImageDraw.Draw(Image.new("L", (1, 1)))
    l, t, r, b = probe.textbbox((0, 0), text, font=font)
    w, h = r - l, b - t
    img = Image.new("L", (w, h), 0)
    ImageDraw.Draw(img).text((-l, -t), text, font=font, fill=255)
    # 二值化：JPEG 那边只有前景/背景两色，灰度没有意义。
    return img.point(lambda v: 255 if v > 96 else 0).convert("1"), w, h


def pack(img, w, h):
    stride = (w + 7) // 8
    out = bytearray(stride * h)
    px = img.load()
    for y in range(h):
        for x in range(w):
            if px[x, y]:
                out[y * stride + (x >> 3)] |= 0x80 >> (x & 7)
    return out, stride


def main():
    out = [
        "/* 本文件由 tools/gen_statusmask.py 生成，不要手改。",
        " * 改文案就重跑那个脚本。 */",
        '#include "statusmask.h"',
        "",
    ]
    table = []
    for name, text in TEXTS:
        img, w, h = render(text)
        if h > BAND_H:
            sys.exit(f"{name}: 渲染高度 {h} 超过条带的 {BAND_H} 像素")
        data, stride = pack(img, w, h)
        out.append(f"/* {text} */")
        out.append(f"static const uint8_t M_{name}[] = {{")
        for i in range(0, len(data), 16):
            out.append("    " + " ".join(f"0x{b:02X}," for b in data[i:i + 16]))
        out.append("};")
        out.append("")
        table.append((name, w, h, stride, text))

    out.append("const status_mask_t STATUS_MASKS[STATUS_COUNT] = {")
    for name, w, h, stride, text in table:
        out.append(f"    [STATUS_{name}] = {{ M_{name}, {w}, {h}, {stride} }},  /* {text} */")
    out.append("};")
    out.append("")

    path = "main/statusmask.c"
    open(path, "w").write("\n".join(out))
    total = sum(w for _, w, _, _, _ in table)
    print(f"写入 {path}：{len(table)} 条文案，合计像素宽 {total}")
    for name, w, h, _, text in table:
        print(f"  STATUS_{name:<12} {w}×{h}  {text}")


if __name__ == "__main__":
    main()
