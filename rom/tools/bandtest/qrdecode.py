#!/usr/bin/env python3
"""配对页二维码的独立解码验证（#31）。

用设备上同一条编码路径（qrdump -> pairband_show -> jb_encode_rows）生成整张
配对页 JPEG，再用与编码器无关的解码器 zxing-cpp 从 JPEG 里解出 URL。
验的是 JPEG 量化、4:2:0 色度下采样之后的真实像素，而不是理想位图。

另外做两个「手机离得远」的压力：缩到 1/2（模块变 4px）、再叠一次高斯模糊。

用法：qrdecode.py <qrdump> <blank.jpg>
依赖：zxing-cpp、pillow、numpy。系统没装时临时装到别处，不动系统：
    pip install --target /tmp/qrdec zxing-cpp pillow numpy
    PYTHONPATH=/tmp/qrdec python3 qrdecode.py ...
缺依赖时默认跳过并提示；设了 REQUIRE_QR_DECODE=1 则缺依赖算失败。
"""
import os
import random
import subprocess
import sys
import tempfile


def main():
    if len(sys.argv) != 3:
        print(__doc__)
        return 2
    try:
        import numpy as np
        import zxingcpp
        from PIL import Image, ImageFilter
    except ImportError as e:
        msg = f"缺解码依赖（{e}），二维码解码验证未跑"
        if os.environ.get("REQUIRE_QR_DECODE") == "1":
            print("✗ " + msg)
            return 1
        print("⚠ " + msg + "；见本文件头部的临时安装办法")
        return 0

    qrdump, blank = sys.argv[1], sys.argv[2]
    rnd = random.Random(31)
    cases = [("888888", "https://mqtt.stickbox.ai/p?d=a1b2c3d4e5f6"),
             ("000000", "https://mqtt.stickbox.ai/p?d=000000000000"),
             ("776031", "https://mqtt.stickbox.ai/p?d=ffffffffffff"),
             ("123456", "http://192.168.1.20:8080/p?d=0123456789ab")]
    for _ in range(40):
        dev = "".join(rnd.choice("0123456789abcdef") for _ in range(12))
        code = "".join(rnd.choice("0123456789") for _ in range(6))
        cases.append((code, f"https://mqtt.stickbox.ai/p?d={dev}"))

    def decode(img):
        res = zxingcpp.read_barcodes(np.asarray(img.convert("L")))
        return [r.text for r in res if r.format == zxingcpp.BarcodeFormat.QRCode]

    fails = 0
    with tempfile.TemporaryDirectory() as td:
        for code, url in cases:
            out = os.path.join(td, "p.jpg")
            r = subprocess.run([qrdump, blank, code, url, out],
                               capture_output=True, text=True)
            if r.returncode != 0:
                print(f"✗ qrdump 失败 {code} {url}: {r.stderr.strip()}")
                fails += 1
                continue
            full = Image.open(out)
            half = full.resize((full.width // 2, full.height // 2), Image.LANCZOS)
            blur = half.filter(ImageFilter.GaussianBlur(1.0))
            for name, img in (("原尺寸", full), ("1/2 缩小", half), ("1/2+模糊", blur)):
                got = decode(img)
                if got != [url]:
                    print(f"✗ {name} 解码不对：码 {code} 期望 {[url]} 实得 {got}")
                    fails += 1
    n = len(cases)
    if fails:
        print(f"✗ 二维码解码 {fails} 项失败（{n} 组 × 3 种画质）")
        return 1
    print(f"✓ 二维码解码：{n} 组（码×设备ID）× 原尺寸/1⁄2 缩小/1⁄2 缩小+模糊，全部解出完整 URL")
    return 0


if __name__ == "__main__":
    sys.exit(main())
