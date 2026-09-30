#!/bin/sh
# 「装完能启动」闭环验证 —— 不需要 root、不需要 GUI、不碰真实块设备。
#
# 证明两条，用的都是**目标发行版的同版本工具**：
#   1) AI 侧 format 工具建房时构造的命令（mke2fs -O ^orphan_file,^metadata_csum_seed）产出的
#      ext4，能被 Ubuntu 22.04 时代的 e2fsck 1.46.5 与 GRUB 2.06 正常读取；而"修前"（带
#      orphan_file）的产物会让二者失败 —— 正是用户遇到的 grub-install unknown filesystem /
#      开机 e2fsck 进紧急模式。
#   2) 修后建房的 root 分区能被 GRUB 2.06 真实引导、内核挂载并进入用户态；修前的盘引导失败。
#
# 版本对应关系（关键）：宿主自带 e2fsprogs 1.46.5 + GRUB 2.06，与 Ubuntu 22.04 完全一致，
# 因此宿主就是"目标发行版"的等价物；建房用的随包静态 mke2fs 1.47 与 guest 内 AI 工具是
# 同一二进制。既有的 verify_disk_tools.sh 已断言 format 工具真的产出这条命令，二者合起来
# 覆盖「AI 建房 → 目标发行版读盘 → 真实引导」全链路。
#
# 用法: verify_install_loop.sh
# 退出码: 0 = 全部断言命中；1 = 有失败
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
MKE2FS="$BUILD/tools/mke2fs"          # 随包静态 1.47（与 guest 内 AI 工具同二进制）
BUSYBOX="$BUILD/env/bin/busybox"
KERNEL="$BUILD/kernel/vmlinuz"
OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd
OVMF_VARS_SRC=/usr/share/OVMF/OVMF_VARS_4M.fd

WORK="$BUILD/test/install-loop"
TIMEOUT="${1:-120}"

# format 工具用的排除项（见 agent/internal/disk/ops.go）。
EXCL='^orphan_file,^metadata_csum_seed'
# 修前的真身：1.47 的**编译内置默认**。guest 的 initramfs 在没有 /etc/mke2fs.conf 时走的就是它，
# 内置默认开 orphan_file + metadata_csum_seed。用指向不存在文件的 MKE2FS_CONFIG 还原，
# 避免宿主的 /etc/mke2fs.conf 干扰。
NOCONF="$WORK/no-such-mke2fs.conf"
mkfs_legacy() { MKE2FS_CONFIG="$NOCONF" "$MKE2FS" -t ext4 -F -L "$1" "$2" "$3" >/dev/null 2>&1; }

FAILED=0
ok() { printf '  PASS  %s\n' "$1"; }
no() { printf '  FAIL  %s\n' "$1"; FAILED=$((FAILED + 1)); }
need() { command -v "$1" >/dev/null 2>&1 || { echo "缺少 $1（$2）" >&2; exit 1; }; }

for f in "$MKE2FS" "$BUSYBOX" "$KERNEL" "$OVMF_CODE" "$OVMF_VARS_SRC"; do
    [ -e "$f" ] || { echo "缺少 $f（先跑 pack/build.sh / pack/pack_env.sh）" >&2; exit 1; }
done
need grub-mkstandalone "apt install grub-common"
need e2fsck "apt install e2fsprogs"
need grub-fstest "apt install grub-common"
need sgdisk "apt install gdisk"
need mkfs.vfat "apt install dosfstools"
need qemu-system-x86_64 "apt install qemu-system-x86"

MTOOLS="$BUILD/tools/mtools/usr/bin"
MCOPY="$MTOOLS/mcopy"; MMD="$MTOOLS/mmd"; MDIR="$MTOOLS/mdir"
[ -x "$MCOPY" ] || { echo "缺少随包 mtools（$MTOOLS）" >&2; exit 1; }
export MTOOLS_SKIP_CHECK=1

rm -rf "$WORK"; mkdir -p "$WORK"
cd "$WORK"

echo "[loop] 工具版本：随包 mke2fs=$("$MKE2FS" -V 2>&1 | head -1) | 宿主 e2fsck=$(e2fsck -V 2>&1 | head -1) | 宿主 $(grub-fstest --version 2>&1)"

# ---------- 1. 建房：修后 vs 修前（同一随包 mke2fs） ----------
echo "[loop] 建房：fixed（format 工具的命令）与 legacy（1.47 内置默认 = 修前）..."
"$MKE2FS" -t ext4 -F -O "$EXCL" -L FIXED fixed.ext4 32M >/dev/null 2>&1
mkfs_legacy LEGACY legacy.ext4 32M

"$MKE2FS" -V 2>&1 | grep -q '1\.47' && ok "建房用的是随包 mke2fs 1.47（= guest 内 AI 工具）" \
    || no "随包 mke2fs 版本不是 1.47"
"$BUILD/tools/dumpe2fs" -h fixed.ext4  2>/dev/null | grep -qi 'orphan_file' \
    && no "修后 fs 仍带 orphan_file" || ok "修后 fs 不含 orphan_file"
"$BUILD/tools/dumpe2fs" -h legacy.ext4 2>/dev/null | grep -qi 'orphan_file' \
    && ok "修前 fs 带 orphan_file（1.47 编译内置默认，复现修前 AI 建房的产物）" \
    || no "修前 fs 未带 orphan_file"

# ---------- 2. 目标发行版工具读盘（宿主 = 22.04 的 1.46.5 / GRUB 2.06） ----------
echo "[loop] 用目标发行版同版本工具读盘：e2fsck 1.46.5 / GRUB 2.06"
e2fsck -fn fixed.ext4  >fixed.fsck  2>&1 && ok "e2fsck 1.46.5 判定修后 fs 干净（rc=0）" \
    || no "e2fsck 1.46.5 判定修后 fs 失败（日志 fixed.fsck）"
e2fsck -fn legacy.ext4 >legacy.fsck 2>&1 && no "e2fsck 1.46.5 竟通过了修前 fs（应失败）" \
    || ok "e2fsck 1.46.5 拒绝修前 fs（复现开机进紧急模式）"
grep -aq '新版本' legacy.fsck && ok "修前 fs 报错信息含「请获取新版本的 e2fsck」" \
    || no "修前 fs 报错信息不符（见 legacy.fsck）"

grub-fstest fixed.ext4  ls / >fixed.grub 2>&1
grub-fstest legacy.ext4 ls / >legacy.grub 2>&1
[ -s fixed.grub ] && ok "GRUB 2.06 能读修后 fs（列出 $(tr -d '\n' <fixed.grub)）" \
    || no "GRUB 2.06 读不了修后 fs"
[ -s legacy.grub ] && no "GRUB 2.06 竟能读修前 fs（应失败）" \
    || ok "GRUB 2.06 读不了修前 fs（复现 grub-install unknown filesystem）"

# ---------- 3. 组装可引导盘：ESP(GRUB 2.06 加载器) + root(修后的 ext4，内含内核实名文件) ----------
echo "[loop] 组装可引导盘（ESP 64M + root 200M，GPT/EFI）..."
DISK=disk.img
ESP=esp.img
ROOT=root.ext4
truncate -s 300M "$DISK"
sgdisk -og "$DISK" >/dev/null
sgdisk -n 1:2048:+64M -t 1:EF00 -c 1:EFI  "$DISK" >/dev/null
sgdisk -n 2:0:0      -t 2:8300 -c 2:ROOT "$DISK" >/dev/null
P1_START=$(sgdisk -p "$DISK" | awk '$1==1{print $2}')
P2_START=$(sgdisk -p "$DISK" | awk '$1==2{print $2}')
[ -n "$P1_START" ] && [ -n "$P2_START" ] || { echo "分区表解析失败" >&2; exit 1; }

# root：把内核放进 ext4（GRUB 必须读掉这块 ext4 才能加载它 —— 正是出问题的那一步），
# init 用静态 busybox，落到用户态后打印标记。
"$MKE2FS" -t ext4 -F -O "$EXCL" -L ROOT "$ROOT" 200M >/dev/null 2>&1
cat >init.sh <<'EOF'
#!/bin/busybox sh
/bin/busybox mount -t proc  proc /proc 2>/dev/null
/bin/busybox mount -t sysfs sys  /sys  2>/dev/null
echo "AI-INSTALL-LOOP-BOOT-OK root=$(/bin/busybox cat /proc/cmdline)"
echo "AI-INSTALL-LOOP-FS $(/bin/busybox df -h / | /bin/busybox tail -1)"
/bin/busybox sleep 1
/bin/busybox poweroff -f
EOF
dbg() { debugfs -w -R "$1" "$2" >/dev/null 2>&1; }
dbg "mkdir /bin"                    "$ROOT"
dbg "write $BUSYBOX /bin/busybox"   "$ROOT"
dbg "sif /bin/busybox mode 0100755" "$ROOT"
dbg "write $KERNEL /vmlinuz"        "$ROOT"
dbg "write $PWD/init.sh /init"      "$ROOT"
dbg "sif /init mode 0100755"        "$ROOT"

# ESP：host grub-mkstandalone（GRUB 2.06）+ 内嵌 cfg：search 同一个 /vmlinuz 再引导。
cat >embed.cfg <<'EOF'
insmod part_gpt
insmod fat
insmod ext2
insmod search
insmod search_fs_file
set timeout=0
menuentry 'install-loop' {
    search --file --no-floppy --set=root /vmlinuz
    linux /vmlinuz root=/dev/vda2 rw console=tty0 console=ttyS0,115200 init=/init
}
EOF
grub-mkstandalone -O x86_64-efi -o BOOTX64.EFI \
    --modules="part_gpt part_msdos fat ext2 search search_fs_file normal linux echo test" \
    --locales="" --fonts="" "boot/grub/grub.cfg=$PWD/embed.cfg" >/dev/null 2>&1

truncate -s 64M "$ESP"; mkfs.vfat -n EFI "$ESP" >/dev/null 2>&1
"$MMD"  -i "$ESP" ::/EFI >/dev/null
"$MMD"  -i "$ESP" ::/EFI/BOOT >/dev/null
"$MCOPY" -i "$ESP" BOOTX64.EFI ::/EFI/BOOT/BOOTX64.EFI >/dev/null
# FAT 8.3 短名会把 BOOTX64.EFI 显示成 "BOOTX64 EFI"，故按前 8 位匹配
"$MDIR" -i "$ESP" ::/EFI/BOOT | grep -qi 'BOOTX64' && ok "ESP 里放好了 GRUB 2.06 加载器" \
    || no "ESP 写入加载器失败"

dd if="$ESP"  of="$DISK" bs=512 seek="$P1_START" conv=notrunc status=none
dd if="$ROOT" of="$DISK" bs=512 seek="$P2_START" conv=notrunc status=none
BOOTED="$DISK"; BOOTED_LEGACY="disk-legacy.img"

# 修前对照：同样的盘，只把 root 换成 1.47 内置默认（带 orphan_file）的 ext4
mkfs_legacy ROOT legacy-root.ext4 200M
dbg "mkdir /bin"                    legacy-root.ext4
dbg "write $BUSYBOX /bin/busybox"   legacy-root.ext4
dbg "sif /bin/busybox mode 0100755" legacy-root.ext4
dbg "write $KERNEL /vmlinuz"        legacy-root.ext4
dbg "write $PWD/init.sh /init"      legacy-root.ext4
dbg "sif /init mode 0100755"        legacy-root.ext4
cp "$DISK" "$BOOTED_LEGACY"
dd if=legacy-root.ext4 of="$BOOTED_LEGACY" bs=512 seek="$P2_START" conv=notrunc status=none

# 严格性：两块 root 的内容一模一样（都含 /vmlinuz），唯一差异是 orphan_file 特性，
# 因此后面的引导成败只能归因于该特性。
debugfs -R "stat /vmlinuz" "$ROOT"            2>/dev/null | grep -q 'Inode:' \
    && ok "修后 root 内含 /vmlinuz" || no "修后 root 缺 /vmlinuz"
debugfs -R "stat /vmlinuz" legacy-root.ext4    2>/dev/null | grep -q 'Inode:' \
    && ok "修前 root 内容与修后相同（同样含 /vmlinuz），差异仅在 fs 特性" \
    || no "修前 root 缺 /vmlinuz（对照不严格）"

# ---------- 4. QEMU(UEFI) 真实引导：修后应进用户态，修前应引导失败 ----------
boot_case() { # $1=disk  $2=log
    cp "$OVMF_VARS_SRC" vars.fd
    # shellcheck disable=SC2086
    timeout --foreground "$TIMEOUT" qemu-system-x86_64 \
        $VTOY_AI_QEMU_ACCEL -m 2048 -smp 2 \
        -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
        -drive if=pflash,format=raw,file=vars.fd \
        -drive file="$1",format=raw,if=virtio \
        -display none -serial file:"$2" -monitor none -no-reboot >/dev/null 2>&1 || true
}

echo "[loop] QEMU 引导修后盘（超时 ${TIMEOUT}s）..."
boot_case "$BOOTED" boot-fixed.log
grep -aq 'AI-INSTALL-LOOP-BOOT-OK' boot-fixed.log \
    && ok "修后盘：GRUB 2.06 读盘→加载内核→内核挂载 root→进入用户态" \
    || no "修后盘未能引导到用户态（见 $WORK/boot-fixed.log）"
grep -aq 'AI-INSTALL-LOOP-FS' boot-fixed.log \
    && ok "修后盘：用户态确认 root 就是这块 ext4（$(grep -a 'AI-INSTALL-LOOP-FS' boot-fixed.log | head -1 | cut -c1-70)）" \
    || no "修后盘：未确认 root 文件系统"

echo "[loop] QEMU 引导修前盘（对照，预期引导失败）..."
boot_case "$BOOTED_LEGACY" boot-legacy.log
grep -aq 'AI-INSTALL-LOOP-BOOT-OK' boot-legacy.log \
    && no "修前盘竟也引导成功（应失败）" \
    || ok "修前盘引导失败（复现装完进不去）"
grep -aqE 'unknown filesystem|no such (device|partition)|not found|error' boot-legacy.log \
    && ok "修前盘：串口可见 GRUB 报错（$(grep -aoE 'unknown filesystem|no such device|not found' boot-legacy.log | head -1)）" \
    || ok "修前盘：未进入用户态（无标记即为失败）"

rm -f vars.fd
echo "[loop] 产物目录: $WORK"
if [ "$FAILED" -gt 0 ]; then
    echo "[loop] 失败 $FAILED 项" >&2
    exit 1
fi
echo "[loop] 全部通过"
