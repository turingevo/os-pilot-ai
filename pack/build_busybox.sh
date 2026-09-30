#!/bin/sh
# 构建 x86_64 静态 busybox（AI 环境 initramfs 用）
# 用法: build_busybox.sh [源码目录] [输出目录]
# 环境变量:
#   VTOY_AI_BUILD_DIR      输出目录（默认见 defaults.sh）
#   VTOY_AI_BUSYBOX_SRC    现成的 busybox 源码目录；不提供则下载官方源码包并校验 SHA256
#   VTOY_AI_BUSYBOX_MIRROR 源码包下载前缀（默认 busybox.net 官方）
set -e

. "$(dirname "$0")/defaults.sh"

OUT="${2:-$VTOY_AI_BUILD_DIR}"
SRC="${1:-${VTOY_AI_BUSYBOX_SRC:-}}"
BB_VER="1.36.1"
# 固定版本与校验和：换镜像或内容被篡改都只会得到与官方一致的文件
BB_SHA="b8cc24c9574d809e7279c3be349795c5d5ceb6fdf19ca709f80cde50e47de314"
MIRROR="${VTOY_AI_BUSYBOX_MIRROR:-https://busybox.net/downloads}"

mkdir -p "$OUT"
[ -n "$SRC" ] || SRC="$OUT/busybox-$BB_VER"

if [ ! -d "$SRC" ]; then
    command -v curl >/dev/null || { echo "[busybox] 需要 curl（或显式给出源码目录）" >&2; exit 1; }
    TBZ="$OUT/dl/busybox-$BB_VER.tar.bz2"
    if [ ! -f "$TBZ" ] || ! echo "$BB_SHA  $TBZ" | sha256sum -c --quiet 2>/dev/null; then
        echo "[busybox] 下载: $MIRROR/busybox-$BB_VER.tar.bz2"
        mkdir -p "$OUT/dl"
        curl -fL --retry 3 -C - -o "$TBZ" "$MIRROR/busybox-$BB_VER.tar.bz2"
        echo "$BB_SHA  $TBZ" | sha256sum -c || {
            echo "[busybox] SHA256 校验失败: $TBZ" >&2
            exit 1
        }
    fi
    echo "[busybox] 解压到 $SRC"
    tar -xjf "$TBZ" -C "$OUT"
fi
[ -d "$SRC" ] || { echo "busybox 源码目录不存在: $SRC" >&2; exit 1; }

WORK="$OUT/busybox-src"
rm -rf "$WORK"
echo "[busybox] 复制源码到 $WORK ..."
cp -a "$SRC" "$WORK"

cd "$WORK"
make distclean >/dev/null 2>&1 || true
make defconfig >/dev/null

# 静态链接 + 关闭 cpio/去掉不必要特性
sed -i 's/^# CONFIG_STATIC is not set/CONFIG_STATIC=y/' .config
sed -i 's/^CONFIG_TC=y/# CONFIG_TC is not set/' .config

# Unicode 直通：defconfig 默认 LAST_SUPPORTED_WCHAR=767（只认到 U+02FF），汉字/日文/
# 西里尔等一律被替换成 '?'（libbb/unicode.c 的 subst 分支）。放宽上限到 0x2FFFF 并
# 打开宽字符/组合字符表，ls/find 等 applet 才能原样输出 UTF-8。
# 注意: kconfig 整型值只写十进制（0x 字面量会导致 silentoldconfig 中断构建）。
sed -i 's/^CONFIG_LAST_SUPPORTED_WCHAR=.*/CONFIG_LAST_SUPPORTED_WCHAR=196607/' .config
sed -i 's/^# CONFIG_UNICODE_WIDE_WCHARS is not set/CONFIG_UNICODE_WIDE_WCHARS=y/' .config
sed -i 's/^# CONFIG_UNICODE_COMBINING_WCHARS is not set/CONFIG_UNICODE_COMBINING_WCHARS=y/' .config

# 并行度：默认 nproc；若高并发下偶发编译器段错误（硬件抖动），可 VTOY_AI_BUILD_JOBS=8 降载
make -j"${VTOY_AI_BUILD_JOBS:-$(nproc)}" >/dev/null

# 自检：非 ASCII 文件名必须原样输出（防 unicode 配置再次漂移成 '?' 替换）
_tmpd=$(mktemp -d)
mkdir "$_tmpd/中文自检"
"$WORK/busybox" ls "$_tmpd" | grep -q "中文自检" || {
    echo "[busybox] 自检失败: 非 ASCII 文件名被改写，检查 unicode 相关 .config" >&2
    rm -rf "$_tmpd"
    exit 1
}
rm -rf "$_tmpd"

ls -la "$WORK/busybox"
file "$WORK/busybox"
echo "[busybox] 完成: $WORK/busybox"
