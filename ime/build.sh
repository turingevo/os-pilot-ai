#!/bin/sh
# 构建本地控制台拼音输入法 helper（libgooglepinyin 的静态封装）。
#
# 产物（默认 $BUILD/ime 下）：
#   pinyin-ime       静态链接的 helper（stdio 协议见 pinyin-ime.cpp 头部注释）
#   dict_pinyin.dat  系统词典（随 libgooglepinyin0 发行，Apache-2.0，见 ime/LICENSE）
#
# 用法: build.sh [构建目录]
# 环境变量:
#   VTOY_AI_BUILD_DIR   构建目录（默认见 pack/defaults.sh）
#   VTOY_AI_IME_MIRROR  镜像前缀（默认清华镜像；官方 archive 同样可用）
set -e

. "$(dirname "$0")/../pack/defaults.sh"

BUILD="${1:-$VTOY_AI_BUILD_DIR}"
AI_DIR="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$AI_DIR/ime/pinyin-ime.cpp"

MIRROR="${VTOY_AI_IME_MIRROR:-https://mirrors.tuna.tsinghua.edu.cn/ubuntu}"
RT_DEB="libgooglepinyin0_0.1.2-7_amd64.deb"
RT_SHA="5817fc017afefacf7c9e005d21757ac6bafd4620ff98381893a905f7da65d69b"
DEV_DEB="libgooglepinyin0-dev_0.1.2-7_amd64.deb"
DEV_SHA="c3e4df3faf63c7c54d63e3ad0f70e19e0842251161ee5326577faea5d76ad3fe"
URL_BASE="$MIRROR/pool/universe/libg/libgooglepinyin"

DL="$BUILD/dl"
IDIR="$BUILD/ime"
EXTRACT="$IDIR/src"

[ -f "$SRC" ] || { echo "[ime] 缺少源码 $SRC" >&2; exit 1; }
command -v dpkg-deb >/dev/null || { echo "[ime] 需要 dpkg-deb" >&2; exit 1; }
command -v g++ >/dev/null || { echo "[ime] 需要 g++" >&2; exit 1; }

fetch_deb() { # $1=url $2=sha256 $3=输出路径
    if [ -f "$3" ] && echo "$2  $3" | sha256sum -c --quiet 2>/dev/null; then
        echo "[ime] 已存在: $(basename "$3")"
        return 0
    fi
    echo "[ime] 下载: $1"
    curl -fL --retry 3 -C - -o "$3" "$1"
    echo "$2  $3" | sha256sum -c --quiet || {
        echo "[ime] SHA256 校验失败: $3" >&2
        rm -f "$3"
        exit 1
    }
}

mkdir -p "$DL" "$IDIR"
fetch_deb "$URL_BASE/$RT_DEB" "$RT_SHA" "$DL/$RT_DEB"
fetch_deb "$URL_BASE/$DEV_DEB" "$DEV_SHA" "$DL/$DEV_DEB"

rm -rf "$EXTRACT"
mkdir -p "$EXTRACT"
dpkg-deb -x "$DL/$RT_DEB" "$EXTRACT/rt"
dpkg-deb -x "$DL/$DEV_DEB" "$EXTRACT/dev"

echo "[ime] 编译 pinyin-ime（静态链接）..."
g++ -O2 -static -s -o "$IDIR/pinyin-ime" "$SRC" \
    -I "$EXTRACT/dev/usr/include" \
    "$EXTRACT/dev/usr/lib/x86_64-linux-gnu/libgooglepinyin.a"

cp "$EXTRACT/rt/usr/lib/x86_64-linux-gnu/googlepinyin/data/dict_pinyin.dat" "$IDIR/dict_pinyin.dat"

# 冒烟：跑一遍协议（词典在 /tmp 建用户词库，避免污染产物目录）
echo "[ime] 冒烟测试（期望输出 R ready 与含「帮我装系统」的候选行）:"
printf 'S\tbangwozhuangxitong\nA\t0\nR\nQ\n' | "$IDIR/pinyin-ime" "$IDIR/dict_pinyin.dat" /tmp/pinyin-ime-smoke-user.dat
rm -f /tmp/pinyin-ime-smoke-user.dat

echo "[ime] 完成:"
ls -la "$IDIR/pinyin-ime" "$IDIR/dict_pinyin.dat"
