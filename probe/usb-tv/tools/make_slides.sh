#!/bin/sh
# 生成三张测试幻灯片，编译时由 EMBED_FILES 进 flash。
#
# 刻意让三张的字节数明显不同：文件长度一变，FAT 链长和目录项里的
# size 字段就都变了，这正是"电视会不会重读 FAT"要观察的东西。
# 三张的主色也完全不同，隔着客厅一眼就能分辨换没换。
set -e
cd "$(dirname "$0")/../assets"

IM=convert
command -v magick >/dev/null 2>&1 && IM="magick"

mk() {  # mk <n> <bgcolor> <fgcolor> <quality> <extra-op>
    $IM -size 1280x720 "xc:$2" \
        -fill "$3" -gravity center \
        -pointsize 420 -annotate +0-40 "$1" \
        -pointsize 56  -annotate +0+250 "ESP32 VDISK  SLIDE $1" \
        $5 -quality "$4" "slide$1.jpg"
}

# 1: 纯色低细节 → 最小
mk 1 '#12355b' '#ffffff' 80 ''
# 2: 加噪声 → 中等
mk 2 '#7a1f2b' '#ffe08a' 85 '-attenuate 1.2 +noise Gaussian'
# 3: 加密集纹理 + 高质量 → 最大
mk 3 '#1b5e20' '#eaffea' 95 '-attenuate 2.5 +noise Uniform'

ls -l slide1.jpg slide2.jpg slide3.jpg
echo
echo "字节数必须互不相同，否则「电视是否重读 FAT」这个实验观察不到差别。"
