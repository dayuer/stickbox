#!/bin/sh
# 核对 usbdev.c 里标了 [待验证] 的三处 API。
# 在 `idf.py reconfigure` 之后跑一次（它会把 managed_components 拉下来）。
#
#   source ~/esp/esp-idf-v5.5/export.sh
#   idf.py reconfigure
#   ./tools/check-api.sh
set -u
cd "$(dirname "$0")/.."

hr() { printf '\n──── %s ────\n' "$1"; }
ok() { printf '  ✓ %s\n' "$1"; }
no() { printf '  ✗ %s\n' "$1"; }

if [ -z "${IDF_PATH:-}" ]; then
    echo "先 source \$IDF_PATH/export.sh"
    exit 1
fi

MC=managed_components
[ -d "$MC" ] || { echo "没有 $MC —— 先跑 idf.py reconfigure"; exit 1; }

hr "0. 有没有误把 esp_tinyusb 拉进来（它会和我们的 tud_msc_* 撞符号）"
if ls -d "$MC"/*esp_tinyusb* >/dev/null 2>&1; then
    no "存在 $(ls -d "$MC"/*esp_tinyusb*)"
    echo "    它的 tinyusb_msc.c 会定义同名回调。确认它没被任何组件 REQUIRES，"
    echo "    并且 CONFIG_TINYUSB_MSC_ENABLED=n；否则链接会报 duplicate symbol。"
else
    ok "没有 esp_tinyusb，只有底层 tinyusb"
fi

TUSB=$(ls -d "$MC"/*tinyusb* 2>/dev/null | grep -v esp_tinyusb | head -1)
[ -n "$TUSB" ] && ok "tinyusb 组件目录：$TUSB" || no "找不到 tinyusb 组件目录"

hr "1. usb_phy_config_t 的字段名（usbdev_start 用）"
PHY="$IDF_PATH/components/usb/include/esp_private/usb_phy.h"
[ -f "$PHY" ] || PHY=$(find "$IDF_PATH/components" -name usb_phy.h 2>/dev/null | head -1)
if [ -n "$PHY" ] && [ -f "$PHY" ]; then
    ok "$PHY"
    sed -n '/typedef struct {/,/} usb_phy_config_t;/p' "$PHY" | sed 's/^/    /'
    echo "    期望看到：controller / target / otg_mode / otg_speed"
    grep -q 'usb_new_phy' "$PHY" && ok "usb_new_phy() 存在" || no "没有 usb_new_phy()"
else
    no "找不到 usb_phy.h —— 路径可能变了，自己 grep usb_new_phy"
fi

hr "2. tud_connect / tud_disconnect（软拔插用）"
if [ -n "$TUSB" ]; then
    grep -rn --include=*.h -E '\btud_(dis)?connect[[:space:]]*\(' "$TUSB" | sed 's/^/    /' | head -6
    grep -rq --include=*.h '\btud_disconnect[[:space:]]*(' "$TUSB" \
        && ok "tud_disconnect 已声明" || no "找不到 tud_disconnect —— 软拔插那条路得改"
fi

hr "3. tud_msc_set_sense 的签名与 SCSI 常量"
if [ -n "$TUSB" ]; then
    grep -rn --include=*.h 'tud_msc_set_sense' "$TUSB" | sed 's/^/    /' | head -4
    for c in SCSI_SENSE_NOT_READY SCSI_SENSE_UNIT_ATTENTION SCSI_SENSE_DATA_PROTECT \
             SCSI_SENSE_ILLEGAL_REQUEST; do
        grep -rq --include=*.h "$c" "$TUSB" && ok "$c" || no "$c 不存在"
    done
fi

hr "4. tud_init 的签名（我们调 tud_init(0)）"
[ -n "$TUSB" ] && grep -rn --include=*.h -E '\btud_init[[:space:]]*\(' "$TUSB" | sed 's/^/    /' | head -4

hr "5. 我们的 tusb_config.h 真的被用上了吗"
echo "    编译后应该能在 build/ 里看到 CFG_TUD_MSC=1 生效。"
echo "    最直接的验证：编过之后 nm -C build/*.elf | grep tud_msc_read10_cb"
echo "    只应出现一次，且来自 usbdev.c。"

printf '\n全部 ✓ 才动手烧。有 ✗ 就先改 usbdev.c 对应那处。\n'
