#!/bin/sh
# 生成 AI 环境 mini-ISO（T1 入口）：拷到 Ventoy 数据分区（part1）根目录即作为普通镜像启动。
# ISO 内结构：/boot/vmlinuz + /boot/initrd + /EFI/BOOT/BOOTX64.EFI（GRUB standalone 引导器）
#            + /ventoy.dat（标记 Ventoy 兼容：Ventoy 直接链式加载本 ISO 的 EFI 引导器）
# 用法: make_iso.sh [输出 ISO] [内核 vmlinuz] [initrd]
# 内核默认取 fetch_kernel.sh 的产物 $BUILD/kernel/vmlinuz（Ubuntu 26.04 发行版内核）
# 环境变量:
#   VTOY_AI_BUILD_DIR   构建目录（默认见 defaults.sh；内核/initrd/输出 ISO 都从这里取）
#   VTOY_AI_CMDLINE       内核命令行（默认串口在前、tty0 在后 ⇒ /dev/console 落在显示器，
#                         真机直接可用；QEMU 无头抓串口日志时把 ttyS0 挪到最后一个覆盖）
#   VTOY_AI_CMDLINE_EXTRA 追加到命令行末尾的自定义参数（测试用，如给某个 ISO 副本打标记）
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
OUT="${1:-$BUILD/test/0-OS-PILOT-AI.iso}"
KERNEL="${2:-$BUILD/kernel/vmlinuz}"
INITRD="${3:-$BUILD/initrd-ai.cpio.gz}"
# 最后一个 console= 决定 /dev/console（即 [init]/agent 的界面位置）：真机默认显示器优先；
# 内核 printk 不受影响，两路各收各的（串口仍能拿到内核日志）。
CMDLINE="${VTOY_AI_CMDLINE:-console=ttyS0,115200 console=tty0 loglevel=3 rdinit=/init vtoy_ai=1}"
if [ -n "${VTOY_AI_CMDLINE_EXTRA:-}" ]; then
    CMDLINE="$CMDLINE ${VTOY_AI_CMDLINE_EXTRA}"
fi

for f in "$KERNEL" "$INITRD"; do
    [ -f "$f" ] || { echo "[make_iso] 缺少文件: $f" >&2; exit 1; }
done
command -v grub-mkstandalone >/dev/null || { echo "[make_iso] 需要 grub-mkstandalone（grub-common）" >&2; exit 1; }
command -v genisoimage >/dev/null || { echo "[make_iso] 需要 genisoimage" >&2; exit 1; }
. "$(dirname "$0")/find_mtools.sh"

WORK="$BUILD/test/iso-staging"
TREE="$WORK/iso"

rm -rf "$WORK"
mkdir -p "$TREE/boot/grub" "$TREE/EFI/BOOT"

# ---------- 内核与 initramfs ----------
cp "$KERNEL" "$TREE/boot/vmlinuz"
cp "$INITRD" "$TREE/boot/initrd"

# ---------- 引导配置 ----------
# OSPILOT.MRK 是本 ISO 的唯一标记：GRUB 用它锁定卷，避免误取内置硬盘上的 /boot/vmlinuz。
cat > "$WORK/embed.cfg" <<EOF
serial --unit=0 --speed=115200
terminal_input console serial
terminal_output console serial
set root=
search --no-floppy --file --set=root /OSPILOT.MRK
if [ -z "\$root" ]; then
    echo "AI 环境：未找到镜像卷（缺少 /OSPILOT.MRK）"
    sleep 30
fi
linux /boot/vmlinuz $CMDLINE
initrd /boot/initrd
boot
EOF
cp "$WORK/embed.cfg" "$TREE/boot/grub/grub.cfg"

: > "$TREE/OSPILOT.MRK"
: > "$TREE/ventoy.dat"

# ---------- EFI 引导器（GRUB standalone，内嵌引导配置）----------
grub-mkstandalone -O x86_64-efi -o "$WORK/BOOTX64.EFI" \
    --modules="linux iso9660 fat ext2 part_msdos part_gpt search search_fs_file search_label normal serial echo sleep test ls cat configfile boot halt reboot" \
    "boot/grub/grub.cfg=$WORK/embed.cfg"

cp "$WORK/BOOTX64.EFI" "$TREE/EFI/BOOT/BOOTX64.EFI"

# ---------- El Torito EFI 入口镜像（FAT）----------
# UEFI 规范要求 El Torito EFI 引导镜像为 FAT（固件/ Ventoy 从该镜像里取 \EFI\BOOT\BOOTX64.EFI）。
# 与 LiveCD/livecd.sh 的 efi.img 做法一致，只是用 mtools 免 root 写入。
EFI_IMG="$WORK/efi.img"
EFI_MB=$(( ($(stat -c %s "$WORK/BOOTX64.EFI") + 1048575) / 1048576 + 2 ))
dd if=/dev/zero of="$EFI_IMG" bs=1M count="$EFI_MB" status=none
mkfs.vfat -n OSPILOTBOOT "$EFI_IMG" >/dev/null
"$MMD" -i "$EFI_IMG" ::/EFI ::/EFI/BOOT
"$MCOPY" -i "$EFI_IMG" "$WORK/BOOTX64.EFI" ::/EFI/BOOT/BOOTX64.EFI
cp "$EFI_IMG" "$TREE/EFI/BOOT/efi.img"

# ---------- 生成 ISO ----------
# 所有路径控制为 8.3 名字（GRUB 的 iso9660 驱动不认 Joliet/RockRidge）。
rm -f "$OUT"
mkdir -p "$(dirname "$OUT")"
genisoimage -quiet -R -J -allow-lowercase \
    -V OS-PILOT-AI -A "OS Pilot AI" -p "OS Pilot AI" -sysid OSPILOT \
    -eltorito-alt-boot -e EFI/BOOT/efi.img -no-emul-boot -c EFI/BOOT/boot.cat \
    -o "$OUT" "$TREE"

echo "[make_iso] 完成: $OUT"
echo "[make_iso] 拷贝到 Ventoy 数据分区根目录（建议名 0-OS-PILOT-AI.iso，靠前缀排到菜单最前）"
ls -la "$OUT"
