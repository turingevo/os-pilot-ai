#!/bin/sh
# 造一块空白虚拟磁盘（全零、无分区表），作为受守卫磁盘工具的靶盘。
# 与 payload 盘不同：它没有任何 Ventoy/系统数据，可随意分区、格式化、写备份。
#
# 用法: make_blank_disk.sh [输出镜像] [大小 MiB]
# 环境变量:
#   VTOY_AI_BUILD_DIR  构建目录（默认见 defaults.sh；默认镜像放在 $BUILD/test/ 下）
#
# 注意: 已存在的同名文件会被**覆盖清空**——这正是"把靶盘重置回空白"的手段。
set -e

. "$(dirname "$0")/defaults.sh"

IMG="${1:-$VTOY_AI_BUILD_DIR/test/scratch-disk.img}"
MB="${2:-2048}"

case "$MB" in
    ''|*[!0-9]*) echo "错误: 大小必须是整数 MiB（当前 '$MB'）" >&2; exit 1 ;;
esac
[ "$MB" -gt 0 ] || { echo "错误: 大小必须为正" >&2; exit 1; }

mkdir -p "$(dirname "$IMG")"
rm -f "$IMG"
truncate -s "${MB}M" "$IMG"

echo "[make_blank_disk] 完成: $IMG（${MB}MiB，全零、无分区表）"
ls -la "$IMG"
