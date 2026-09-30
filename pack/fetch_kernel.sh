#!/bin/sh
# 下载并裁剪 Ubuntu 26.04 LTS (resolute) 发行版内核，供 AI 环境使用。
#
# 产物（默认 $BUILD/kernel 下）：
#   vmlinuz                        发行版 vmlinuz（稳定路径，make_iso/run_qemu 默认读取）
#   config-<ver>                   发行版内核配置（参考）
#   dist/                          两个 deb 原样解包（boot/ + 完整模块树，供调试）
#   modules-mini/lib/modules/<ver>/  按 kernel-modules.list 裁剪的模块树（.ko 已解压，
#                                    busybox modprobe 不认 .ko.zst，故打包期解压）
#
# 用法: fetch_kernel.sh [构建目录]
# 环境变量:
#   VTOY_AI_BUILD_DIR    构建目录（默认见 defaults.sh）
#   VTOY_AI_KERNEL_MIRROR  镜像前缀（默认清华镜像；官方 archive 慢但同样可用）
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="${1:-$VTOY_AI_BUILD_DIR}"
AI_DIR="$(cd "$(dirname "$0")/.." && pwd)"
LIST="$AI_DIR/pack/kernel-modules.list"

KVER="7.0.0-34-generic"
KPKG="7.0.0-34.34"
MIRROR="${VTOY_AI_KERNEL_MIRROR:-https://mirrors.tuna.tsinghua.edu.cn/ubuntu}"

# 固定版本与校验和：镜像内容即使变动也只会得到与官方一致的文件
IMG_DEB="linux-image-${KVER}_${KPKG}_amd64.deb"
IMG_SHA="12357ef72548fa1854c16b98f43a6a6070496af50f120afa601d278c61d2fc20"
MOD_DEB="linux-modules-${KVER}_${KPKG}_amd64.deb"
MOD_SHA="fd207ceefc3b1d817439d1ebf1013d00689c60fa90a04f7b8b4ef39d3f01ac3f"
IMG_URL="$MIRROR/pool/main/l/linux-signed/$IMG_DEB"
MOD_URL="$MIRROR/pool/main/l/linux/$MOD_DEB"

DL="$BUILD/dl"
KDIR="$BUILD/kernel"
DIST="$KDIR/dist"
MINI="$KDIR/modules-mini"

command -v dpkg-deb >/dev/null || { echo "[fetch_kernel] 需要 dpkg-deb" >&2; exit 1; }
command -v depmod  >/dev/null || { echo "[fetch_kernel] 需要 depmod（kmod）" >&2; exit 1; }
command -v zstd    >/dev/null || { echo "[fetch_kernel] 需要 zstd（apt install zstd）" >&2; exit 1; }
[ -f "$LIST" ] || { echo "[fetch_kernel] 缺少模块清单 $LIST" >&2; exit 1; }

fetch_deb() { # $1=url $2=sha256 $3=输出路径
    if [ -f "$3" ] && echo "$2  $3" | sha256sum -c --quiet 2>/dev/null; then
        echo "[fetch_kernel] 已存在: $(basename "$3")"
        return 0
    fi
    echo "[fetch_kernel] 下载: $1"
    curl -fL --retry 3 -C - -o "$3" "$1"
    echo "$2  $3" | sha256sum -c --quiet || {
        echo "[fetch_kernel] SHA256 校验失败: $3" >&2
        rm -f "$3"
        exit 1
    }
}

mkdir -p "$DL" "$KDIR"
fetch_deb "$IMG_URL" "$IMG_SHA" "$DL/$IMG_DEB"
fetch_deb "$MOD_URL" "$MOD_SHA" "$DL/$MOD_DEB"

# ---------- 解包 ----------
echo "[fetch_kernel] 解包 deb -> $DIST"
rm -rf "$DIST"
mkdir -p "$DIST"
dpkg-deb -x "$DL/$IMG_DEB" "$DIST"
dpkg-deb -x "$DL/$MOD_DEB" "$DIST"

VMLINUZ="$KDIR/vmlinuz"
cp "$DIST/boot/vmlinuz-$KVER" "$VMLINUZ"
cp "$DIST/boot/config-$KVER" "$KDIR/config-$KVER"

# 26.04 采用 merged-usr：模块树在 usr/lib/modules/；depmod -b 需要 lib/ 视角
[ -e "$DIST/lib" ] || ln -sfn usr/lib "$DIST/lib"

# ---------- 全量模块树 depmod（生成 modules.dep 供依赖求值）----------
echo "[fetch_kernel] 全量模块树 depmod ..."
depmod -b "$DIST" "$KVER" 2>&1 | grep -v 'modules.order' || true

# ---------- 依赖闭包 + 裁剪 ----------
rm -rf "$MINI"
mkdir -p "$MINI/lib/modules/$KVER"
echo "[fetch_kernel] 按 $LIST 求依赖闭包并解压 .ko ..."
grep -v '^#' "$LIST" | sed 's/[[:space:]]*$//' | grep -v '^$' > "$KDIR/kernel-modules.txt"
while read -r m; do
    if ! deps=$(modprobe -d "$DIST" -S "$KVER" --show-depends "$m" 2>/dev/null); then
        echo "[fetch_kernel] 模块清单中的模块不存在: $m" >&2
        exit 1
    fi
    echo "$deps" | awk '/^insmod /{print $2}'
done < "$KDIR/kernel-modules.txt" | sort -u > "$KDIR/closure.txt"

PREFIX="$DIST/lib/modules/$KVER/kernel/"
while read -r f; do
    rel=${f#$PREFIX}
    out="$MINI/lib/modules/$KVER/kernel/${rel%.zst}"
    mkdir -p "$(dirname "$out")"
    zstd -dc "$f" > "$out"
done < "$KDIR/closure.txt"

cp "$DIST/lib/modules/$KVER/modules.builtin" "$MINI/lib/modules/$KVER/"
depmod -b "$MINI" "$KVER" 2>&1 | grep -v 'modules.order' || true

echo "[fetch_kernel] 完成:"
echo "  vmlinuz      $VMLINUZ ($(du -h "$VMLINUZ" | cut -f1))"
echo "  模块清单     $(wc -l < "$KDIR/kernel-modules.txt") 个模块 -> 闭包 $(wc -l < "$KDIR/closure.txt") 个"
echo "  裁剪模块树   $MINI ($(du -sh "$MINI/lib/modules/$KVER" | cut -f1))"
