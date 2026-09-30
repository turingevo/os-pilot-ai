#!/bin/sh
# 定位 mtools（mcopy/mmd/mformat）。用法: . "$(dirname "$0")/find_mtools.sh"
# 查找顺序: PATH → $VTOY_AI_MTOOLS_DIR → $VTOY_AI_BUILD_DIR/tools/mtools/usr/bin（get_mtools.sh 装到这里）
. "$(dirname "$0")/defaults.sh"

MTOOLS_BIN=""
if command -v mcopy >/dev/null 2>&1; then
    MTOOLS_BIN="$(dirname "$(command -v mcopy)")"
else
    for d in "${VTOY_AI_MTOOLS_DIR:-}" "$VTOY_AI_BUILD_DIR/tools/mtools/usr/bin"; do
        if [ -n "$d" ] && [ -x "$d/mcopy" ]; then
            MTOOLS_BIN="$d"
            break
        fi
    done
fi
if [ -z "$MTOOLS_BIN" ]; then
    echo "错误: 未找到 mtools（mcopy）。" >&2
    echo "  有 root: sudo apt install mtools" >&2
    echo "  无 root: sh \"$(dirname "$0")\"/get_mtools.sh（下载 deb 解包到构建目录）" >&2
    exit 1
fi
MCOPY="$MTOOLS_BIN/mcopy"
MMD="$MTOOLS_BIN/mmd"
MFORMAT="$MTOOLS_BIN/mformat"
MDIR="$MTOOLS_BIN/mdir"
export MCOPY MMD MFORMAT MDIR
