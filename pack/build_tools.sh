#!/bin/sh
# 构建 x86_64 静态工具链（分区 / 格式化 / 备份），供 AI 环境 initramfs 使用
#
# 产物: $VTOY_AI_BUILD_DIR/tools/ —— 全部静态链接、已 strip
#   e2fsprogs : mke2fs e2fsck resize2fs tune2fs dumpe2fs   ext2/3/4 的建/检/调
#   parted    : parted                                    脚本化分区（GPT/MBR）
#   rsync     : rsync                                     增量备份
#
# 用法: build_tools.sh [输出目录]
# 环境变量:
#   VTOY_AI_BUILD_DIR    输出目录（默认见 defaults.sh）
#   VTOY_AI_TOOLS_FORCE  置 1 强制重建（默认产物齐了就跳过，省几分钟编译）
#
# 约定（对齐 build_busybox.sh）:
#   - 源码包下载到 dl/ 并做 SHA256 校验：e2fsprogs 用 kernel.org 官方 sha256sums，
#     rsync/parted 用 Ubuntu .dsc 与 GNU 发布值记录下来的同一份。
#   - 依赖只走源码自带件，不依赖宿主开发包：rsync 用内置 popt/zlib；
#     parted 的 libuuid 用 e2fsprogs 自己那份（因此 e2fsprogs 必须先编）。
#   - 体积优先：-Os + --gc-sections + strip（比默认构建小约 40%，initramfs 直接受益）。
set -e

. "$(dirname "$0")/defaults.sh"

OUT="${1:-$VTOY_AI_BUILD_DIR}"
TOOLS="$OUT/tools"
DL="$OUT/dl"
SRC_ROOT="$OUT/tools-src"
mkdir -p "$TOOLS" "$DL" "$SRC_ROOT"

E2FS_VER="1.47.0"
E2FS_SHA="0b4fe723d779b0927fb83c9ae709bc7b40f66d7df36433bef143e41c54257084"
E2FS_URL="https://cdn.kernel.org/pub/linux/kernel/people/tytso/e2fsprogs/v$E2FS_VER/e2fsprogs-$E2FS_VER.tar.gz"

RSYNC_VER="3.2.7"
RSYNC_SHA="4e7d9d3f6ed10878c58c5fb724a67dacf4b6aac7340b13e488fb2dc41346f2bb"
RSYNC_URL="http://archive.ubuntu.com/ubuntu/pool/main/r/rsync/rsync_$RSYNC_VER.orig.tar.gz"

PARTED_VER="3.6"
PARTED_SHA="3b43dbe33cca0f9a18601ebab56b7852b128ec1a3df3a9b30ccde5e73359e612"
PARTED_URL="https://ftp.gnu.org/gnu/parted/parted-$PARTED_VER.tar.xz"

# 期望产物；都在且未强制重建就直接收工
WANT="mke2fs e2fsck resize2fs tune2fs dumpe2fs parted rsync"
if [ "${VTOY_AI_TOOLS_FORCE:-0}" != "1" ]; then
    all=1
    for b in $WANT; do [ -x "$TOOLS/$b" ] || all=0; done
    if [ "$all" = "1" ]; then
        echo "[tools] 已是最新（如需重建: VTOY_AI_TOOLS_FORCE=1 $0）"
        ls -la "$TOOLS"
        exit 0
    fi
fi

command -v curl >/dev/null || { echo "[tools] 需要 curl" >&2; exit 1; }

# fetch <缓存文件名> <URL> <SHA256>
fetch() {
    f="$DL/$1"; u="$2"; s="$3"
    if [ ! -f "$f" ] || ! echo "$s  $f" | sha256sum -c --quiet 2>/dev/null; then
        echo "[tools] 下载: $u"
        curl -fL --retry 3 -C - -o "$f" "$u"
        echo "$s  $f" | sha256sum -c || { echo "[tools] SHA256 校验失败: $f" >&2; exit 1; }
    fi
}

# 每次从源码包重新解压，保证构建可复现
unpack() {  # unpack <目录名> <包文件> <解压参数>
    rm -rf "$SRC_ROOT/$1"
    mkdir -p "$SRC_ROOT"
    tar "$3" "$DL/$2" -C "$SRC_ROOT"
    [ -d "$SRC_ROOT/$1" ] || { echo "[tools] 解压后找不到 $1" >&2; exit 1; }
}

JOBS="$(nproc)"
CFLAGS_OPT="-Os -ffunction-sections -fdata-sections"
LDFLAGS_OPT="-static -Wl,--gc-sections"

# ---------- e2fsprogs（同时产出 parted 需要的 libuuid）----------
fetch "e2fsprogs-$E2FS_VER.tar.gz" "$E2FS_URL" "$E2FS_SHA"
unpack "e2fsprogs-$E2FS_VER" "e2fsprogs-$E2FS_VER.tar.gz" "-xzf"
E2FS_SRC="$SRC_ROOT/e2fsprogs-$E2FS_VER"
echo "[tools] 编译 e2fsprogs $E2FS_VER ..."
cd "$E2FS_SRC"
CFLAGS="$CFLAGS_OPT" ./configure \
    --disable-nls --disable-uuidd --disable-fuse2fs --disable-e2initrd-helper \
    --enable-libuuid --enable-libblkid >/dev/null
make -j"$JOBS" LDFLAGS="$LDFLAGS_OPT" >/dev/null
# 取产物并 strip：显式列出（e2fsprogs 的二进制分散在 misc/ e2fsck/ resize/ 三个目录）
copybin() {  # copybin <源码内相对路径> <产物名>
    cp "$E2FS_SRC/$1" "$TOOLS/$2"
    strip "$TOOLS/$2"
}
copybin misc/mke2fs mke2fs
copybin misc/tune2fs tune2fs
copybin misc/dumpe2fs dumpe2fs
copybin e2fsck/e2fsck e2fsck
copybin resize/resize2fs resize2fs

# ---------- parted（libuuid 用 e2fsprogs 自带那份）----------
# 注意: parted 用 libtool，libtool 会把 LDFLAGS 里的 -static 吃掉（实际链接行里没有它，
# 结果 -lblkid/glibc 全走动态）。必须用 libtool 自己的 -all-static，并配 --disable-shared
# 让它只用 libparted.a。曾经踩过：只看 `file` 输出没看退出码，误以为静态通过。
fetch "parted-$PARTED_VER.tar.xz" "$PARTED_URL" "$PARTED_SHA"
unpack "parted-$PARTED_VER" "parted-$PARTED_VER.tar.xz" "-xf"
PARTED_SRC="$SRC_ROOT/parted-$PARTED_VER"
echo "[tools] 编译 parted $PARTED_VER ..."
cd "$PARTED_SRC"
CPPFLAGS="-I$E2FS_SRC/lib/uuid" CFLAGS="$CFLAGS_OPT" LDFLAGS="-static -L$E2FS_SRC/lib/uuid" \
    ./configure --disable-nls --disable-device-mapper --without-readline \
    --disable-shared --enable-static >/dev/null
make -j"$JOBS" LDFLAGS="-static -L$E2FS_SRC/lib/uuid -all-static -Wl,--gc-sections" >/dev/null
cp "$PARTED_SRC/parted/parted" "$TOOLS/parted"
strip "$TOOLS/parted"

# ---------- rsync（内置 popt/zlib；xxhash/zstd/lz4 缺头文件，显式关掉）----------
fetch "rsync-$RSYNC_VER.tar.gz" "$RSYNC_URL" "$RSYNC_SHA"
unpack "rsync-$RSYNC_VER" "rsync-$RSYNC_VER.tar.gz" "-xzf"
RSYNC_SRC="$SRC_ROOT/rsync-$RSYNC_VER"
echo "[tools] 编译 rsync $RSYNC_VER ..."
cd "$RSYNC_SRC"
CFLAGS="$CFLAGS_OPT" ./configure \
    --with-included-popt --with-included-zlib \
    --disable-openssl --disable-iconv --disable-acl-support --disable-xattr-support \
    --disable-ipv6 --disable-xxhash --disable-zstd --disable-lz4 >/dev/null
make -j"$JOBS" LDFLAGS="$LDFLAGS_OPT" >/dev/null
cp "$RSYNC_SRC/rsync" "$TOOLS/rsync"
strip "$TOOLS/rsync"

# ---------- 校验：必须是静态链接，且能跑起来 ----------
echo "[tools] 产物校验:"
for b in $WANT; do
    [ -x "$TOOLS/$b" ] || { echo "[tools] 缺少产物: $b" >&2; exit 1; }
    if ! file "$TOOLS/$b" | grep -q "statically linked"; then
        echo "[tools] $b 不是静态链接（initramfs 无 libc，会跑不起来）" >&2
        exit 1
    fi
    printf '  %-11s %9s bytes  静态\n' "$b" "$(stat -c %s "$TOOLS/$b")"
done
rm -rf "$SRC_ROOT"
echo "[tools] 完成: $TOOLS"
