#!/bin/sh
# 用 T13a 的黄金帧包验证设备端的时钟带拼接。
# 黄金图的时刻是 12:47。
set -e
cd "$(dirname "$0")"
P="${1:-../../../../esp-airprint-T13a/docs/design/preview}"
cc -I../../main -O1 -Wall -Werror -o bandtest bandtest.c ../../main/framepack_band.c ../../main/framepack_meta.c
cc -I../../main -O1 -Wall -Werror -o metatest metatest.c ../../main/framepack_meta.c ../../main/framepack_band.c
fail=0
for f in "$P"/*.meta.json; do
    n=$(basename "$f" .meta.json)
    printf '%-12s ' "$n"
    ./bandtest "$f" "$P/$n.base.jpg" "$P/$n.atlas.bin" "$P/$n.spliced.jpg" 12 47 | tail -1 || fail=1
done
./metatest "$P"/01-*.meta.json
exit $fail
