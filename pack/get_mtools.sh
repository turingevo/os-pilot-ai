#!/bin/sh
# 免 root 获取 mtools（mcopy/mmd：往 FAT 镜像里写文件用，不依赖 mount）
# 原理: apt-get download + dpkg -x 解包到构建目录（仓库外）
# 用法: sh get_mtools.sh   （结果: $VTOY_AI_BUILD_DIR/tools/mtools/usr/bin/mcopy）
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
DEST="$BUILD/tools/mtools"

if [ -x "$DEST/usr/bin/mcopy" ]; then
    echo "[get_mtools] 已存在: $DEST/usr/bin/mcopy"
    exit 0
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cd "$TMP"
apt-get download mtools
mkdir -p "$DEST"
dpkg -x mtools_*.deb "$DEST"
echo "[get_mtools] 完成: $DEST/usr/bin/mcopy（用 VTOY_AI_MTOOLS_DIR 或 find_mtools.sh 定位）"
