#!/bin/sh
# stickbox-agent 一键安装（macOS / Linux，不需要 sudo）。
#
#   curl -fsSL https://raw.githubusercontent.com/dayuer/stickbox/main/server/go/cmd/stickbox-agent/install.sh | sh
#
# 或者下载后：
#   sh install.sh [--version vX.Y.Z] [--dir ~/.local/bin]
#
# 只做三件事：下载对应平台的二进制、按 Release 的 SHA256SUMS 校验、放进
# ~/.local/bin。不会自动配对，也不会安装后台服务——那两步要用户自己确认。
set -eu

REPO="${STICKBOX_REPO:-dayuer/stickbox}"
VERSION="${STICKBOX_VERSION:-}"
INSTALL_DIR="${STICKBOX_INSTALL_DIR:-$HOME/.local/bin}"

usage() {
    cat <<'EOF'
用法：install.sh [选项]

  --version vX.Y.Z   指定版本（默认取 GitHub 最新 Release）
  --dir PATH         安装目录（默认 ~/.local/bin）
  -h, --help         显示帮助

Windows 暂不支持这个脚本；请从 GitHub Release 手动下载
stickbox-agent-windows-amd64.exe，并自行放进 PATH。
EOF
}

die() {
    printf '错误：%s\n' "$*" >&2
    exit 1
}

while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            [ $# -ge 2 ] || die "--version 需要一个版本号"
            VERSION="$2"
            shift 2
            ;;
        --dir)
            [ $# -ge 2 ] || die "--dir 需要一个目录"
            INSTALL_DIR="$2"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            die "未知参数：$1（用 --help 查看用法）"
            ;;
    esac
done

case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    MINGW*|MSYS*|CYGWIN*)
        die "Windows 请从 GitHub Release 手动下载 stickbox-agent-windows-amd64.exe"
        ;;
    *)
        die "不支持的平台：$(uname -s)（只支持 macOS / Linux）"
        ;;
esac

case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) die "不支持的架构：$(uname -m)" ;;
esac

command -v curl >/dev/null 2>&1 || die "需要 curl，请先安装"

if [ -z "$VERSION" ]; then
    VERSION=$(curl -fsSL -H 'Accept: application/vnd.github+json' \
        "https://api.github.com/repos/$REPO/releases/latest" \
        | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
        | head -n 1)
    [ -n "$VERSION" ] || die "取不到最新 Release；用 --version vX.Y.Z 指定版本"
fi

asset="stickbox-agent-$os-$arch"
base="https://github.com/$REPO/releases/download/$VERSION"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

printf '下载 %s（%s）…\n' "$asset" "$VERSION"
curl -fsSL -o "$tmp/$asset" "$base/$asset" || die "下载 $asset 失败：$base/$asset"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || die "下载 SHA256SUMS 失败"

expected=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/SHA256SUMS")
[ -n "$expected" ] || die "SHA256SUMS 里没有 $asset，Release 可能不完整"

if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
else
    die "找不到 sha256sum 或 shasum，无法校验下载"
fi

[ "$actual" = "$expected" ] || die "SHA256 校验失败：期望 $expected，实际 $actual"

mkdir -p "$INSTALL_DIR"
install -m 0755 "$tmp/$asset" "$INSTALL_DIR/stickbox-agent" 2>/dev/null \
    || { cp "$tmp/$asset" "$INSTALL_DIR/stickbox-agent" && chmod 0755 "$INSTALL_DIR/stickbox-agent"; }

printf '已安装：%s\n' "$INSTALL_DIR/stickbox-agent"

case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
        printf '\n注意：%s 不在 PATH 里。把它加进去：\n' "$INSTALL_DIR"
        printf '  export PATH="%s:%s"\n' "$INSTALL_DIR" "\$PATH"
        ;;
esac

cat <<EOF

下一步（按顺序）：
  1) stickbox-agent pair <配对码>     # 配对码显示在电视/屏幕上
  2) stickbox-agent install           # 安装后台服务（登录后自动上报）
  3) stickbox-agent doctor            # 逐项检查，出问题会给中文建议

升级后重新运行 stickbox-agent install，让后台服务使用新二进制。
EOF
