#!/usr/bin/env bash
# 在 ESP-IDF 容器里执行发布固件构建。
#
#   tools/release_build.sh <tag> <ota_key.pem>
#
# 关键点：必须用独立的 -DSDKCONFIG=build-release/sdkconfig。
# 已有的 rom/sdkconfig 会把 sdkconfig.release 里的签名开关盖掉，
# 构建仍然成功，但 OTA 签名校验是关的。这里构建后再 grep 一遍配置。
set -euo pipefail

TAG="${1:?用法: release_build.sh <tag> <ota_key.pem>}"
KEY="${2:?用法: release_build.sh <tag> <ota_key.pem>}"

cd "$(dirname "$0")/.."   # rom/

# 编译前静态检查：MQTT 重连绝不能再用 esp_mqtt_set_config（会清空 TLS 配置）。
python3 tools/check_mqtt_config.py

if [ ! -f sdkconfig.release ]; then
    echo "rom/sdkconfig.release 不存在：T9/M0 尚未合并，发布构建无法开启 OTA 签名" >&2
    exit 1
fi
if ! grep -q 'ota_0' partitions.csv || ! grep -q 'ota_1' partitions.csv; then
    echo "partitions.csv 没有 ota_0/ota_1：T9/M0 的分区表尚未合并" >&2
    exit 1
fi
if [ ! -f "$KEY" ]; then
    echo "签名私钥不存在: $KEY" >&2
    exit 1
fi

if ! command -v idf.py >/dev/null 2>&1; then
    if [ -n "${IDF_PATH:-}" ] && [ -f "$IDF_PATH/export.sh" ]; then
        # shellcheck disable=SC1091
        source "$IDF_PATH/export.sh"
    else
        echo "idf.py 不在 PATH，且 IDF_PATH 未设置" >&2
        exit 1
    fi
fi

install -m 600 "$KEY" ota_key.pem
trap 'rm -f ota_key.pem' EXIT

# 发布构建固定用 dependencies.lock 里的版本，不去 registry 查新版。
# 新版检查会在空 sdkconfig 上求值组件的可选依赖条件，触发
# MissingKconfigError: LV_USE_FREETYPE。
export IDF_COMPONENT_CHECK_NEW_VERSION=0

rm -rf build-release
mkdir -p build-release

idf.py -B build-release \
    -DSDKCONFIG=build-release/sdkconfig \
    -DSDKCONFIG_DEFAULTS="sdkconfig.defaults;sdkconfig.release" \
    -DPROJECT_VER="$TAG" \
    build

# 防呆：编进去的服务器地址必须是官方主机。cloud_creds.h 是 gitignore 的
# 本地文件，开发机指向旧服务器时构建照样成功，设备上板才 DNS 失败。
EXPECTED_HOST="${STICKBOX_EXPECTED_HOST:-mqtt.stickbox.ai}"
python3 tools/check_embedded_hosts.py build-release/stickbox.bin "$EXPECTED_HOST"

# 防呆：签名开关必须真的落到这个独立 sdkconfig 里。
grep -q '^CONFIG_SECURE_SIGNED_ON_UPDATE_NO_SECURE_BOOT=y$' build-release/sdkconfig || {
    echo "build-release/sdkconfig 没有 CONFIG_SECURE_SIGNED_ON_UPDATE_NO_SECURE_BOOT=y" >&2
    exit 1
}
grep -q '^CONFIG_SECURE_SIGNED_APPS_NO_SECURE_BOOT=y$' build-release/sdkconfig || {
    echo "build-release/sdkconfig 没有 CONFIG_SECURE_SIGNED_APPS_NO_SECURE_BOOT=y" >&2
    exit 1
}

# 构建产物必须真的带签名，不只是 sdkconfig 开关打开。
espsecure.py extract_public_key --version 2 --keyfile ota_key.pem build-release/ota_pub.pem
espsecure.py verify_signature --version 2 --keyfile build-release/ota_pub.pem build-release/stickbox.bin
rm -f build-release/ota_pub.pem

# ESP-IDF 的 app 文件名来自 project(stickbox)，是 stickbox.bin；
# flasher_args.json 指向它。另存一份 app.bin 满足发布产物的通用命名。
APP_BIN="build-release/stickbox.bin"
cp "$APP_BIN" build-release/app.bin
python tools/check_app_version.py "$APP_BIN" "$TAG"
python tools/make_ota_manifest.py "$APP_BIN" "$TAG" build-release/ota-manifest.json

for f in bootloader/bootloader.bin partition_table/partition-table.bin \
         ota_data_initial.bin stickbox.bin app.bin flasher_args.json ota-manifest.json; do
    test -f "build-release/$f" || { echo "缺少发布产物: build-release/$f" >&2; exit 1; }
done

echo "发布固件构建完成: $TAG"
