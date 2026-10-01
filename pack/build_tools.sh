#!/bin/sh
# 构建 x86_64 静态工具链（分区 / 格式化 / 备份），供 AI 环境 initramfs 使用
#
# 产物: $VTOY_AI_BUILD_DIR/tools/ —— 全部静态链接、已 strip
#   e2fsprogs : mke2fs e2fsck resize2fs tune2fs dumpe2fs   ext2/3/4 的建/检/调
#   parted    : parted                                    脚本化分区（GPT/MBR）
#   rsync     : rsync                                     增量备份
#   exfatprogs: mkfs.exfat fsck.exfat                     exFAT 的建/检（Win+mac+Android 通用）
#   f2fs-tools: mkfs.f2fs fsck.f2fs                       f2fs 的建/检（Android）
#   ntfs-3g   : mkntfs ntfsfix                            NTFS 的建/修（挂载读写用内核 ntfs3，不装 FUSE 驱动）
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
#   - exfatprogs/f2fs-tools 的可选外部依赖（uuid/blkid/lz4/lzo/selinux）一律关掉，
#     保持零依赖；f2fs-tools 与 ntfs-3g 的上游 tarball 不含 configure，需要宿主 autoreconf -fi
#     （ntfs-3g 另需一份 libgcrypt 桩宏，见其构建段注释）。
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

EXFATPROGS_VER="1.4.3"
EXFATPROGS_SHA="57226a8ec1bfbce06d68a42cde8cd980414a9457882e691fbce4a4f86c8d5f08"
EXFATPROGS_URL="http://archive.ubuntu.com/ubuntu/pool/main/e/exfatprogs/exfatprogs_${EXFATPROGS_VER}.orig.tar.xz"

F2FS_VER="1.16.0"
F2FS_SHA="fe25b17422f278e5fc0c6ae9977d814fe3122b5bc82dd92a58296fad57263d9c"
F2FS_URL="http://archive.ubuntu.com/ubuntu/pool/universe/f/f2fs-tools/f2fs-tools_${F2FS_VER}.orig.tar.xz"

NTFS_VER="2021.8.22"
NTFS_SHA="5cb9fa93bf2b9685e3f1b598861f6082786e76562989a5752c7379dbe0e989a2"
NTFS_URL="http://archive.ubuntu.com/ubuntu/pool/main/n/ntfs-3g/ntfs-3g_${NTFS_VER}.orig.tar.gz"

# 期望产物；都在且未强制重建就直接收工
WANT="mke2fs e2fsck resize2fs tune2fs dumpe2fs parted rsync mkfs.exfat fsck.exfat mkfs.f2fs fsck.f2fs mkntfs ntfsfix"
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

# ---------- exfatprogs（exFAT：Windows / macOS / Android 13+ 都能读写的通用数据盘格式）----------
# 同 parted 的坑：libtool 链接时会吃掉 LDFLAGS 里的 -static，必须在 make 时补 -all-static，
# 否则产物是动态链接（initramfs 里没有 libc，到 guest 里跑不起来）。
fetch "exfatprogs-$EXFATPROGS_VER.tar.xz" "$EXFATPROGS_URL" "$EXFATPROGS_SHA"
unpack "exfatprogs-$EXFATPROGS_VER" "exfatprogs-$EXFATPROGS_VER.tar.xz" "-xf"
EXFAT_SRC="$SRC_ROOT/exfatprogs-$EXFATPROGS_VER"
echo "[tools] 编译 exfatprogs $EXFATPROGS_VER ..."
cd "$EXFAT_SRC"
CFLAGS="$CFLAGS_OPT" LDFLAGS="-static -Wl,--gc-sections" \
    ./configure --disable-shared --enable-static >/dev/null
make -j"$JOBS" LDFLAGS="-static -all-static -Wl,--gc-sections" >/dev/null
cp "$EXFAT_SRC/mkfs/mkfs.exfat" "$TOOLS/mkfs.exfat"
cp "$EXFAT_SRC/fsck/fsck.exfat" "$TOOLS/fsck.exfat"
strip "$TOOLS/mkfs.exfat" "$TOOLS/fsck.exfat"

# ---------- f2fs-tools（f2fs：Android 内部存储与 OTG 盘）----------
# 上游 tarball 不带 configure，需要 autoreconf -fi；可选外部依赖全部关掉，
# 保证产物零依赖（uuid 关掉后 mkfs.f2fs 自己生成随机 UUID）。
command -v autoreconf >/dev/null || {
    echo "[tools] 需要 autoreconf（apt install autoconf automake libtool）" >&2; exit 1; }
fetch "f2fs-tools-$F2FS_VER.tar.xz" "$F2FS_URL" "$F2FS_SHA"
unpack "f2fs-tools-$F2FS_VER" "f2fs-tools-$F2FS_VER.tar.xz" "-xf"
F2FS_SRC="$SRC_ROOT/f2fs-tools-$F2FS_VER"
echo "[tools] 编译 f2fs-tools $F2FS_VER ..."
cd "$F2FS_SRC"
autoreconf -fi >/dev/null
ac_cv_lib_uuid_uuid_clear=no ac_cv_header_uuid_uuid_h=no \
CFLAGS="$CFLAGS_OPT" LDFLAGS="-static -Wl,--gc-sections" \
    ./configure --disable-shared --enable-static \
    --without-blkid --without-lz4 --without-lzo2 --without-selinux >/dev/null
make -j"$JOBS" LDFLAGS="-static -all-static -Wl,--gc-sections" >/dev/null
cp "$F2FS_SRC/mkfs/mkfs.f2fs" "$TOOLS/mkfs.f2fs"
cp "$F2FS_SRC/fsck/fsck.f2fs" "$TOOLS/fsck.f2fs"
strip "$TOOLS/mkfs.f2fs" "$TOOLS/fsck.f2fs"

# ---------- ntfs-3g（只取 mkntfs + ntfsfix）----------
# 挂载读写由内核 ntfs3 负责，这里不装 FUSE 驱动（--disable-ntfs-3g），所以源码里内置的
# libfuse-lite 只会用来满足编译期引用，产物里没有 fuse。
# 两个坑：
#   1. tarball 不带 configure，要 autoreconf -fi；上游 configure.ac 里的 AM_PATH_LIBGCRYPT
#      只在 --enable-crypto 时才真正执行，但宏本身必须在 autoconf 期可展开——宿主没装
#      libgcrypt 的开发件（缺 libgcrypt.m4）时用下面这份"只走未找到分支"的桩宏顶上。
#   2. ntfsfix 依赖 default device io ops，不能加 --disable-device-default-io-ops。
fetch "ntfs-3g-$NTFS_VER.tar.gz" "$NTFS_URL" "$NTFS_SHA"
unpack "ntfs-3g-$NTFS_VER" "ntfs-3g-$NTFS_VER.tar.gz" "-xzf"
NTFS_SRC="$SRC_ROOT/ntfs-3g-$NTFS_VER"
echo "[tools] 编译 ntfs-3g $NTFS_VER（mkntfs / ntfsfix）..."
cd "$NTFS_SRC"
mkdir -p "$SRC_ROOT/ntfs-stub-m4"
cat > "$SRC_ROOT/ntfs-stub-m4/libgcrypt-stub.m4" <<'M4'
dnl AM_PATH_LIBGCRYPT 的构建期桩：展开成"未找到"分支即可。
dnl 上游只在 --enable-crypto 时才真正探测 libgcrypt，本脚本从不启用加密件。
AC_DEFUN([AM_PATH_LIBGCRYPT], [m4_default([$3], [:])])
M4
if ! ACLOCAL="aclocal -I $SRC_ROOT/ntfs-stub-m4" autoreconf -fi >/dev/null 2>&1; then
    echo "[tools] ntfs-3g autoreconf 失败" >&2
    exit 1
fi
CFLAGS="$CFLAGS_OPT" LDFLAGS="-static -Wl,--gc-sections" \
    ./configure --disable-ntfs-3g --enable-ntfsprogs --with-fuse=internal \
    --disable-posix-acls --disable-xattr-mappings --disable-plugins \
    --disable-mtab --disable-shared --enable-static >/dev/null
make -j"$JOBS" LDFLAGS="-static -all-static -Wl,--gc-sections" >/dev/null
cp "$NTFS_SRC/ntfsprogs/mkntfs" "$TOOLS/mkntfs"
cp "$NTFS_SRC/ntfsprogs/ntfsfix" "$TOOLS/ntfsfix"
strip "$TOOLS/mkntfs" "$TOOLS/ntfsfix"

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
