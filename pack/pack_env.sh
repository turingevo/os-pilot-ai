#!/bin/sh
# 组装 AI 环境 initramfs（cpio.gz）
# 内容: init + busybox(含全部 applet 符号链接) + os-pilot-ai + 配置
# 用法: pack_env.sh [输出文件] [busybox 路径]
# 环境变量:
#   VTOY_AI_BUILD_DIR  构建目录（默认见 defaults.sh；initramfs 与 busybox 都从这里取）
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
OUT="${1:-$BUILD/initrd-ai.cpio.gz}"
BUSYBOX="${2:-$BUILD/busybox-src/busybox}"
AI_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$(cd "$(dirname "$OUT")" && pwd)"
ENV_DIR="$BUILD_DIR/env"

if [ ! -x "$BUSYBOX" ]; then
    echo "busybox 不存在或不可执行: $BUSYBOX" >&2
    exit 1
fi
if [ ! -x "$AI_DIR/agent/os-pilot-ai" ]; then
    echo "agent 未构建，请先运行 $AI_DIR/agent/build.sh" >&2
    exit 1
fi

rm -rf "$ENV_DIR"
mkdir -p "$ENV_DIR"/bin "$ENV_DIR"/sbin "$ENV_DIR"/proc "$ENV_DIR"/sys "$ENV_DIR"/dev \
         "$ENV_DIR"/tmp "$ENV_DIR"/etc "$ENV_DIR"/iso "$ENV_DIR"/ventoy/ai

# busybox + applet 符号链接（必须用相对链接，--install 会生成指向构建目录的绝对链接）
cp "$BUSYBOX" "$ENV_DIR/bin/busybox"
chmod 755 "$ENV_DIR/bin/busybox"
(
    cd "$ENV_DIR/bin"
    for a in $("$ENV_DIR/bin/busybox" --list); do
        [ "$a" = "busybox" ] && continue
        ln -sf busybox "$a"
    done
)

# 分区/格式化/备份工具链（pack/build_tools.sh 的产物；可选，缺失时只提示）
# 装到 /bin 而不是 /sbin：run_command 的 PATH 是 /bin:/sbin:/usr/bin:/usr/sbin，
# /bin 优先；而且 busybox 的 mke2fs/mkfs.ext2/fsck 符号链接也在 /bin，必须盖掉。
# 坑：cp 会穿透符号链接写到 busybox 本体上，所以先 rm 再 cp。
TOOLS_DIR="${VTOY_AI_TOOLS_DIR:-$BUILD/tools}"
TOOLS_LIST="mke2fs e2fsck resize2fs tune2fs dumpe2fs parted rsync"
if [ -x "$TOOLS_DIR/mke2fs" ]; then
    for t in $TOOLS_LIST; do
        [ -x "$TOOLS_DIR/$t" ] || continue
        rm -f "$ENV_DIR/bin/$t"
        cp "$TOOLS_DIR/$t" "$ENV_DIR/bin/$t"
        chmod 755 "$ENV_DIR/bin/$t"
    done
    # 别名：mke2fs/e2fsck 按 argv[0] 判断做哪种文件系统，必须有这些名字
    for a in mkfs.ext2 mkfs.ext3 mkfs.ext4 fsck.ext2 fsck.ext3 fsck.ext4; do
        case "$a" in
            mkfs.*) ln -sf mke2fs "$ENV_DIR/bin/$a" ;;
            fsck.*) ln -sf e2fsck "$ENV_DIR/bin/$a" ;;
        esac
    done
    echo "[pack_env] 已装入工具链: $(cd "$TOOLS_DIR" && ls | tr '\n' ' ')"
else
    echo "[pack_env] 警告: 未找到工具链 $TOOLS_DIR（可运行 pack/build_tools.sh 生成）" >&2
fi

# AI 组件
cp "$AI_DIR/agent/os-pilot-ai" "$ENV_DIR/ventoy/ai/os-pilot-ai"
cp "$AI_DIR/init/init" "$ENV_DIR/init"
cp "$AI_DIR/init/mount_payload.sh" "$ENV_DIR/ventoy/ai/mount_payload.sh"
cp "$AI_DIR/init/udhcpc.script" "$ENV_DIR/ventoy/ai/udhcpc.script"
cp "$AI_DIR/config/ai.json.example" "$ENV_DIR/ventoy/ai/ai.json.example"
cp "$AI_DIR/pack/kernel-modules.list" "$ENV_DIR/ventoy/ai/kernel-modules.list"
chmod 755 "$ENV_DIR/init" "$ENV_DIR/ventoy/ai/mount_payload.sh" "$ENV_DIR/ventoy/ai/udhcpc.script"
chmod 755 "$ENV_DIR/ventoy/ai/os-pilot-ai"

# 自绘屏幕点阵字体（VTF1；独立数据文件，不嵌入 agent 二进制）
# 缺失时调用 make_font.sh 生成（走 $BUILD/font-cache 或 apt 下载；离线用 VTOY_FONT_HEX）
FONT="$BUILD_DIR/screen/font.bin"
if [ ! -f "$FONT" ]; then
    echo "[pack_env] 点阵字体缺失，调用 make_font.sh 生成..."
    sh "$AI_DIR/pack/make_font.sh" "$FONT"
fi
mkdir -p "$ENV_DIR/ventoy/ai/screen"
cp "$FONT" "$ENV_DIR/ventoy/ai/screen/font.bin"

# 字体许可与来源（GNU Unifont 为 GPL-2.0-or-later；作为独立数据文件与软件聚合分发）
cat > "$ENV_DIR/ventoy/ai/screen/LICENSE.unifont" <<'EOF'
font.bin — GNU Unifont 点阵字体数据（构建期由 unifont.hex 转换为 VTF1）

来源: GNU Unifont (https://unifoundry.com/unifont/)
转换: pack/make_font.sh + tools/make_font.py
许可: GNU General Public License v2 or later (GPL-2.0-or-later)
源码: 字体可修改形式（unifont.hex）可从上述官网免费获取，或用发行版软件源的
      unifont 包获取（pack/make_font.sh 即此法）

本文件是独立的数据文件，与同介质上的软件仅构成聚合分发（mere aggregation），
其许可不影响同介质上其他组件的许可。
许可证全文: https://www.gnu.org/licenses/old-licenses/gpl-2.0.html
EOF
if [ -f /usr/share/common-licenses/GPL-2 ]; then
    cp /usr/share/common-licenses/GPL-2 "$ENV_DIR/ventoy/ai/screen/GPL-2.0.txt"
else
    echo "[pack_env] 提示: 未找到 /usr/share/common-licenses/GPL-2，许可全文链接见 LICENSE.unifont" >&2
fi

# 内置拼音输入法（可选组件，ime/build.sh 的产物；缺失时 agent 自动降级为无输入法）
IME_DIR="${VTOY_AI_IME_DIR:-$BUILD_DIR/ime}"
if [ -x "$IME_DIR/pinyin-ime" ] && [ -f "$IME_DIR/dict_pinyin.dat" ]; then
    mkdir -p "$ENV_DIR/ventoy/ai/ime"
    cp "$IME_DIR/pinyin-ime" "$ENV_DIR/ventoy/ai/ime/pinyin-ime"
    cp "$IME_DIR/dict_pinyin.dat" "$ENV_DIR/ventoy/ai/ime/dict_pinyin.dat"
    chmod 755 "$ENV_DIR/ventoy/ai/ime/pinyin-ime"
else
    echo "[pack_env] 警告: 未找到输入法组件 $IME_DIR/{pinyin-ime,dict_pinyin.dat}，跳过（可运行 ime/build.sh 生成）" >&2
fi

# 发行版内核模块（fetch_kernel.sh 的裁剪产物）
# 注意: .ko 必须是解压态——busybox modprobe 不支持 .ko.zst
KVER="${VTOY_AI_KVER:-7.0.0-34-generic}"
KMOD_DIR="${VTOY_AI_MODULES_DIR:-$BUILD_DIR/kernel/modules-mini}/lib/modules/$KVER"
if [ -d "$KMOD_DIR" ]; then
    mkdir -p "$ENV_DIR/lib/modules"
    cp -a "$KMOD_DIR" "$ENV_DIR/lib/modules/$KVER"
else
    echo "[pack_env] 缺少内核模块树 $KMOD_DIR，请先运行 fetch_kernel.sh" >&2
    exit 1
fi

# 设备节点（内核 devtmpfs 挂载前 init 需要 /dev/console）
mknod -m 600 "$ENV_DIR/dev/console" c 5 1 2>/dev/null || true
mknod -m 666 "$ENV_DIR/dev/null" c 1 3 2>/dev/null || true

# 基础文件
printf 'nameserver 10.0.2.3\n' > "$ENV_DIR/etc/resolv.conf"
printf '/dev/root / ext4 ro 0 0\n' > "$ENV_DIR/etc/fstab"

# mke2fs 默认特性：随包 e2fsprogs 1.47 的**内置默认**会开 orphan_file/metadata_csum_seed，
# 而目标发行版（如 Ubuntu 22.04 的 e2fsck 1.46.5 / GRUB 2.06）读不了带这些特性的 ext4 ——
# 表现为安装器 grub-install 报 "unknown filesystem"（ESP 留空）、装完开机 systemd-fsck 失败。
# 这里随包给一份 1.46 代配置（与 Ubuntu 22.04 的 /etc/mke2fs.conf 一致），保证 guest 内
# 任何 mke2fs 调用都产出目标系统可读的 ext4；disk 工具的 format 另加 -O 显式排除。
cat > "$ENV_DIR/etc/mke2fs.conf" <<'EOF'
[defaults]
	base_features = sparse_super,large_file,filetype,resize_inode,dir_index,ext_attr
	default_mntopts = acl,user_xattr
	enable_periodic_fsck = 0
	blocksize = 4096
	inode_size = 256
	inode_ratio = 16384

[fs_types]
	ext3 = {
		features = has_journal
	}
	ext4 = {
		features = has_journal,extent,huge_file,flex_bg,metadata_csum,64bit,dir_nlink,extra_isize
	}
	small = {
		inode_ratio = 4096
	}
	big = {
		inode_ratio = 32768
	}
	huge = {
		inode_ratio = 65536
	}
EOF

# CA 根证书（https 接口必需；Go 默认读取 /etc/ssl/certs/ca-certificates.crt）
CA_BUNDLE="${VTOY_AI_CA_BUNDLE:-/etc/ssl/certs/ca-certificates.crt}"
if [ -f "$CA_BUNDLE" ]; then
    mkdir -p "$ENV_DIR/etc/ssl/certs"
    cp "$CA_BUNDLE" "$ENV_DIR/etc/ssl/certs/ca-certificates.crt"
else
    echo "[pack_env] 警告: 未找到 CA 证书 $CA_BUNDLE，guest 内 https 请求将失败" >&2
fi

cd "$ENV_DIR"
find . | cpio -o -H newc 2>/dev/null | gzip -c -9 > "$OUT"
cd - >/dev/null

echo "[pack_env] 完成: $OUT"
ls -la "$OUT"
