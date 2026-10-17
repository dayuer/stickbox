#!/usr/bin/env python3
"""渲染预置页面：1920x1080 baseline JPEG，4:2:0，每个 MCU 行一个 restart 段（DRI=120）。

y = 800..959（MCU 行 50..59）是时钟带：这里只留纯背景色，板子每秒重新编码这 10 行
再拼回去。背景色必须和固件 main.c 里 PAGE_BG 表一致。

    python3 -I tools/render_pages.py      # 输出 assets/page{1,2,3}.jpg
"""
import os
from PIL import Image, ImageDraw, ImageFont

FONT = "/System/Library/Fonts/Hiragino Sans GB.ttc"
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "assets")
BAND_Y0, BAND_Y1 = 800, 960  # 必须 16 对齐；改了要同步固件 CLOCK_ROW0 / CLOCK_ROWS

PAGES = [
    ((30, 58, 138), "第 1 页", "订阅用量 87% · 剩余 4 小时 12 分"),
    ((127, 29, 29), "第 2 页", "打印队列 3 份 · 下一份 2 分钟后"),
    ((20, 83, 45), "第 3 页", "今日待办 5 项 · 已完成 2 项"),
]


def render(idx, bg, title, line):
    im = Image.new("RGB", (1920, 1080), bg)
    d = ImageDraw.Draw(im)
    f = lambda px: ImageFont.truetype(FONT, px)
    d.text((80, 70), f"{title}  StickBox 信息屏", font=f(96), fill="white")
    d.text((80, 220), line, font=f(56), fill=(230, 230, 230))
    d.text((80, 330), "ESP32 独立推流 · 1080p MJPEG · 时钟由板子每秒现场编码", font=f(32), fill=(200, 200, 200))
    d.rectangle([80, 420, 1840, 422], fill="white")
    d.rectangle([80, 480, 1280, 540], fill=(255, 255, 255))
    d.rectangle([80, 480, 80 + int(1200 * (0.87, 0.4, 0.4)[idx]), 540], fill=(74, 222, 128))
    d.text((80, 740), "北京时间", font=f(36), fill=(200, 200, 200))
    assert not any(im.getpixel((x, y)) != bg for x in range(0, 1920, 7) for y in range(BAND_Y0, BAND_Y1, 3)), \
        "时钟带里不能有内容"
    p = os.path.join(OUT, f"page{idx + 1}.jpg")
    im.save(p, "JPEG", quality=92, subsampling=2, restart_marker_rows=1, optimize=False, progressive=False)
    print(p, os.path.getsize(p), "bg", bg)


for i, (bg, t, l) in enumerate(PAGES):
    render(i, bg, t, l)
