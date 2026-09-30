#!/bin/sh
# 生成自绘屏幕点阵字体（unifont.hex -> font.bin）
#
# 用法: make_font.sh [输出路径]
#  默认输出: $VTOY_AI_BUILD_DIR/screen/font.bin（仓库外构建产物，随 initramfs 分发）
#
# 环境变量:
#   VTOY_FONT_HEX      已有 unifont.hex 的路径（离线构建；指定后不下载）
#   VTOY_AI_BUILD_DIR  下载与缓存目录（默认见 defaults.sh）
#
# 字体数据来自 GNU Unifont（GPLv2+，含字体嵌入例外条款），仅在构建期由
# 系统软件源下载转换，产物不进入仓库。
set -e

. "$(dirname "$0")/defaults.sh"

AI_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$VTOY_AI_BUILD_DIR"
OUT="${1:-$BUILD_DIR/screen/font.bin}"
HEX="${VTOY_FONT_HEX:-}"

if [ -z "$HEX" ]; then
    CACHE="$BUILD_DIR/font-cache"
    HEX="$CACHE/unifont.hex"
    if [ ! -f "$HEX" ]; then
        echo "[make_font] 下载 unifont 字体包（apt-get download，无需 root）..."
        mkdir -p "$CACHE"
        (cd "$CACHE" && apt-get download unifont >/dev/null 2>&1) || {
            echo "[make_font] apt-get download 失败；可设置 VTOY_FONT_HEX 指定 unifont.hex" >&2
            exit 1
        }
        DEB="$(ls "$CACHE"/unifont_*.deb 2>/dev/null | head -1)"
        [ -n "$DEB" ] || { echo "[make_font] 未下载到 unifont 包" >&2; exit 1; }
        rm -rf "$CACHE/extracted"
        dpkg-deb -x "$DEB" "$CACHE/extracted"
        FOUND="$(find "$CACHE/extracted" -name unifont.hex | head -1)"
        [ -n "$FOUND" ] || { echo "[make_font] 包内未找到 unifont.hex" >&2; exit 1; }
        cp "$FOUND" "$HEX"
    fi
fi

[ -f "$HEX" ] || { echo "[make_font] 字体源不存在: $HEX" >&2; exit 1; }

mkdir -p "$(dirname "$OUT")"
python3 "$AI_DIR/tools/make_font.py" "$HEX" "$OUT"
