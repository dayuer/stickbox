#!/bin/sh
# 主机侧校验：不需要 ESP-IDF，也不需要板子。
# 把 vdisk.c 编到 x86 上导出镜像，再用 test/validate.py（独立按 FAT32 规范写的
# 解析器）检查，终局判据是把文件从簇链里抠出来和原图逐字节比对。
set -e
cd "$(dirname "$0")/.."
cd assets
for n in 1 2 3; do
    objcopy -I binary -O elf64-x86-64 -B i386 "slide$n.jpg" "../test/s$n.o"
done
cd ..
gcc -O1 -Wall -Wextra -Imain -Itest/stubs -o test/harness \
    test/harness.c main/vdisk.c test/s1.o test/s2.o test/s3.o 2>/dev/null
rc=0
for i in 0 1 2; do
    printf '\n═══════ slide%d ═══════\n' $((i + 1))
    ./test/harness $i 2>/dev/null | ./test/validate.py $i || rc=1
done
exit $rc
