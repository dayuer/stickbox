#!/bin/sh
# T13b 的主机侧验证。两组：
#   1) bandtest / metatest —— 拿 T13a 的黄金产物验拼接核心与 meta 解析
#   2) packtest —— 吃真实的 SBF1 帧包，走设备侧全部字节逻辑，和服务端的
#      黄金拼接帧逐字节比对。帧包用 server/go/cmd/packgen 现生成。
#
# 板子不是随时能用，而这些字节级的约定错一个字节在板上就是花屏。能在主机
# 上钉死的就不要留到上板。
set -e
cd "$(dirname "$0")"
CC_ARGS="-I../../main -O1 -Wall -Werror"
cc $CC_ARGS -o bandtest bandtest.c ../../main/framepack_band.c ../../main/framepack_meta.c
cc $CC_ARGS -o metatest metatest.c ../../main/framepack_meta.c ../../main/framepack_band.c
cc $CC_ARGS -o packtest packtest.c ../../main/framepack_band.c ../../main/framepack_meta.c
# 状态页走设备上真正跑的那条路：同一份掩码、同一个 jb_encode_rows、
# 同一个着色器。esp_log.h 用 hoststub 里的桩顶掉。
cc -I../../main -Ihoststub -O1 -Wall -o pagetest pagetest.c ../../main/jband.c ../../main/statusmask.c
cc -I../../main -Ihoststub -O1 -Wall -o pairtest pairtest.c ../../main/pairband.c ../../main/jband.c ../../main/statusmask.c
cc $CC_ARGS -o vertest vertest.c ../../main/framepack_ver.c
# 照片裸扇区存储：板上跑的是同一份 photostore_core.c，掉电/CRC/分配在主机钉死
cc $CC_ARGS -o photostore_test photostore_test.c ../../main/photostore_core.c ../../main/photosha.c
# 清单流式解析 + init_token 判决 + status JSON
cc $CC_ARGS -o photo_core_test photo_core_test.c ../../main/photo_json.c ../../main/photo_policy.c

# 帧包和黄金帧都由 server/go/cmd/packgen 现生成，不依赖任何别人的 worktree
# ——那些目录会被收敛掉，测试不该因此失效。
PK="${1:-/tmp/packgen}"
if [ ! -f "$PK/frame.sbf1" ]; then
    echo "现生成帧包与黄金帧…"
    (cd ../../../server/go && go run ./cmd/packgen -out "$PK" >/dev/null)
fi

echo "== 1. 拼接核心 vs 服务端黄金拼接帧 =="
printf '12:47  '
./bandtest "$PK/meta.json" "$PK/base.jpg" "$PK/atlas.bin" "$PK/golden-1247.jpg" 12 47 | tail -1

echo "== 2. meta 解析的拒绝路径 =="
./metatest "$PK/meta.json" | tail -2

echo "== 3. 端到端：真实 SBF1 容器 vs 服务端黄金拼接帧 =="
for t in 0000 0828 1247 2359; do
    ./packtest "$PK/frame.sbf1" "$PK/golden-$t.jpg" "${t%??}" "${t#??}" | tail -1
done

echo "== 4. 状态页渲染（设备上同一条编码路径）=="
mkdir -p /tmp/pages
./pagetest ../../main/assets/blank.jpg /tmp/pages
for f in /tmp/pages/page-*.jpg; do
    djpeg -outfile /dev/null "$f" 2>/dev/null || { echo "✗ $f 解码失败"; exit 1; }
done
echo "   六张都通过 djpeg 解码"

echo "== 5. 配对页最坏码 + 提示文字 =="
./pairtest ../../main/assets/blank.jpg

echo "== 6. 槽头版本字段解码 =="
./vertest

echo "== 7. 照片存储：格式化/掉电/CRC/扫描 =="
./photostore_test | tail -3

echo "== 8. 照片清单解析 / 初始化授权 / status.photo =="
./photo_core_test | tail -3
