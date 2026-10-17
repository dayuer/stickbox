#!/usr/bin/env bash
# 把指定版本的 esp-web-tools 复制到 web/flash/vendor/。
#
#   web/vendor-update.sh            # 默认 10.4.0
#   web/vendor-update.sh 10.5.0
#
# 烧录页能把固件写进用户的板子，所以运行时不从 CDN 加载脚本：
# 这里从 npm 拉 tarball，先用 registry 公布的 sha512 integrity 校验，
# 再解包 dist/web（install-button.js 及它动态 import 的全部 chunk）。
set -euo pipefail

VERSION="${1:-10.4.0}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$ROOT/web/flash/vendor/esp-web-tools-$VERSION"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "拉取 esp-web-tools@$VERSION 的 registry 元数据…"
curl -fsSL "https://registry.npmjs.org/esp-web-tools/$VERSION" -o "$TMP/meta.json"

TARBALL_URL="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["dist"]["tarball"])' "$TMP/meta.json")"
INTEGRITY="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["dist"]["integrity"])' "$TMP/meta.json")"

echo "下载 tarball…"
curl -fsSL "$TARBALL_URL" -o "$TMP/pkg.tgz"

python3 - "$TMP/pkg.tgz" "$INTEGRITY" <<'PY'
import base64, hashlib, sys

path, integrity = sys.argv[1], sys.argv[2]
algo, _, b64 = integrity.partition("-")
if not b64 or algo not in ("sha512", "sha256"):
    sys.exit(f"无法识别的 integrity：{integrity}")
raw = open(path, "rb").read()
if hashlib.new(algo, raw).digest() != base64.b64decode(b64):
    sys.exit(f"{algo} 校验失败：tarball 与 npm registry 公布的值不一致")
print(f"{algo} integrity 校验通过")
PY

mkdir -p "$TMP/out"
tar -xzf "$TMP/pkg.tgz" -C "$TMP/out"
SRC="$TMP/out/package/dist/web"
if [ ! -f "$SRC/install-button.js" ]; then
  echo "包里没有 dist/web/install-button.js" >&2
  exit 1
fi

rm -rf "$DEST"
mkdir -p "$DEST"
cp -R "$SRC/." "$DEST/"
cp "$TMP/out/package/LICENSE" "$DEST/LICENSE"

echo "已更新 $DEST"
