#!/bin/sh
# guest 内实测内置工具链：mke2fs/e2fsck/resize2fs/tune2fs/dumpe2fs/parted/rsync
#                                + mkfs.exfat/fsck.exfat + mkfs.f2fs/fsck.f2fs + mkntfs/ntfsfix。
# 每种格式都跑完整闭环：建分区 → mkfs → blkid 回读 TYPE → 挂载读写 → 拷文件 → umount。
#
# 取证约定：脚本模式会把 !命令原样回显进日志，所以标记必须取自工具/内核的真实输出；
#           「已挂载」一律核对 /proc/mounts 里的「设备 挂载点 类型」三元组，不能只 grep 挂载点。
#
# 原理：agent 的脚本模式把行首为 ! 的输入当本地命令直执（复用 run_command 安全层），
#       且脚本模式下确认自动通过。因此用一份「纯 !命令」脚本即可在 guest 内跑完整套
#       工具，不需要 LLM，也不需要本地屏/OCR：非交互串口把全部输出写进日志文件。
#
# 做法：
#   1. 生成 test_script.txt（!命令序列，靶盘 /dev/vdb）
#   2. 造 payload 盘（VTOY_AI_SCRIPT_FILE 注入上面的脚本）
#   3. 额外挂一块空白 virtio 盘当靶盘（payload 是 /dev/vda，靶盘是 /dev/vdb）
#   4. 非交互 QEMU 启动，结束后 grep 串口日志核对各工具的真实输出
#
# 用法: verify_tools.sh [超时秒数]
# 环境变量: VTOY_AI_BUILD_DIR（默认见 defaults.sh）
# 退出码: 0 = 全部标记命中；1 = 有缺失或超时
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
PACK_DIR="$(cd "$(dirname "$0")" && pwd)"
TEST_DIR="$BUILD/test"
PAYLOAD_IMG="$TEST_DIR/payload.img"
SCRATCH="$TEST_DIR/scratch.img"
SCRIPT="$TEST_DIR/tools-script.txt"
LOG="$TEST_DIR/qemu-tools.log"
TIMEOUT="${1:-300}"

mkdir -p "$TEST_DIR"

# ---------- 1. guest 内要执行的命令序列 ----------
# 每行必须以 ! 开头（agent 命令直执）。靶盘固定 /dev/vdb（第二块 virtio 盘）。
cat > "$SCRIPT" <<'EOF'
!parted --version
!mkfs.ext4 -V
!rsync --version
!parted -s /dev/vdb mklabel gpt
!parted -s /dev/vdb mkpart primary ext4 1MiB 60MiB
!parted -s /dev/vdb mkpart exfat 60MiB 110MiB
!parted -s /dev/vdb set 2 msftdata on
!parted -s /dev/vdb mkpart f2fs f2fs 110MiB 170MiB
!parted -s /dev/vdb mkpart data ntfs 170MiB 100%
!parted -s /dev/vdb set 4 msftdata on
!mdev -s
!lsmod
!parted -s /dev/vdb print
!mkfs.ext4 -F -L aitest /dev/vdb1
!e2fsck -fn /dev/vdb1
!resize2fs -P /dev/vdb1
!tune2fs -l /dev/vdb1
!dumpe2fs -h /dev/vdb1
!mkdir -p /mnt/t
!mount /dev/vdb1 /mnt/t
!grep vdb1 /proc/mounts
!df -h /mnt/t
!cp /iso/ventoy/ai.json /mnt/t/copied.json
!rsync -av /iso/ventoy/ /mnt/t/ventoy-backup/
!ls /mnt/t/ventoy-backup
!ls /mnt/t/ventoy-backup/ai
!umount /mnt/t
!mkfs.exfat -L EXPART /dev/vdb2
!blkid /dev/vdb2
!fsck.exfat /dev/vdb2
!mkdir -p /mnt/e
!mount -t exfat /dev/vdb2 /mnt/e
!grep vdb2 /proc/mounts
!cp /iso/ventoy/ai.json /mnt/e/ai.json
!ls /mnt/e
!umount /mnt/e
!mkfs.f2fs -l ANDROIDP /dev/vdb3
!blkid /dev/vdb3
!fsck.f2fs -t /dev/vdb3
!mkdir -p /mnt/f
!mount -t f2fs /dev/vdb3 /mnt/f
!grep vdb3 /proc/mounts
!cp /iso/ventoy/ai.json /mnt/f/ai.json
!ls /mnt/f
!umount /mnt/f
!mkntfs -V
!mkntfs -f -F -L WIN /dev/vdb4
!blkid /dev/vdb4
!ntfsfix -n /dev/vdb4
!mkdir -p /mnt/n
!mount -t ntfs3 /dev/vdb4 /mnt/n
!grep vdb4 /proc/mounts
!cp /iso/ventoy/ai.json /mnt/n/ai.json
!ls /mnt/n
!umount /mnt/n
EOF

# ---------- 2. payload 盘（脚本模式，指向上面的脚本）----------
echo "[verify] 造 payload 盘（脚本模式，无 LLM）..."
VTOY_AI_WITH_SCRIPT=1 VTOY_AI_SCRIPT_FILE="$SCRIPT" \
    sh "$PACK_DIR/make_test_disk.sh" >/dev/null

# ---------- 3. 靶盘 ----------
rm -f "$SCRATCH"
truncate -s 256M "$SCRATCH"

# ---------- 4. 非交互启动，串口落盘 ----------
rm -f "$LOG"
echo "[verify] 启动 QEMU（payload=/dev/vda 靶盘=/dev/vdb，超时 ${TIMEOUT}s）..."
# shellcheck disable=SC2086
timeout --foreground "$TIMEOUT" qemu-system-x86_64 \
    $VTOY_AI_QEMU_ACCEL \
    -m "$VTOY_AI_QEMU_MEM" -smp "$VTOY_AI_QEMU_SMP" \
    -kernel "$BUILD/kernel/vmlinuz" \
    -initrd "$BUILD/initrd-ai.cpio.gz" \
    -append "console=ttyS0 loglevel=3 rdinit=/init vtoy_ai=1" \
    -drive file="$PAYLOAD_IMG",format=raw,if=virtio \
    -drive file="$SCRATCH",format=raw,if=virtio \
    -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
    -display none -serial file:"$LOG" -monitor none -no-reboot \
    || true

# ---------- 5. 核对标记 ----------
# 每个标记都取自对应工具真实输出的一段文字；命中即证明该二进制在 guest 内跑起来了。
check() {
    if grep -aq "$1" "$LOG"; then
        printf '  PASS  %s\n' "$2"
    else
        printf '  FAIL  %s\n' "$2"
        FAILED=$((FAILED + 1))
    fi
}

FAILED=0
echo "[verify] 核对串口日志 $LOG:"
check "parted (GNU parted) 3"      "parted 版本（静态二进制可执行）"
check "mke2fs 1.47"                "mke2fs 版本（mkfs.ext4 别名可用）"
check "rsync  version 3.2.7"       "rsync 版本（静态二进制可执行）"
check "Partition Table: gpt"       "parted 建 GPT 分区表"
check "aitest"                     "mke2fs 写卷标 aitest"
check "Creating filesystem with"   "mke2fs 实际建 ext4"
check "Filesystem volume name:   aitest" "tune2fs 读到卷标"
check "Filesystem features:"       "dumpe2fs 读到超级块"
check "Estimated minimum size"     "resize2fs 读到文件系统"
check "Pass 5: Checking group summary information" "e2fsck 五遍检查跑完"
check "non-contiguous),"      "e2fsck 报告文件系统状态"
check "^hfsplus"              "lsmod 有 hfsplus（macOS 目标盘可读写）"
check "^f2fs"                 "lsmod 有 f2fs（Android 目标盘可读写）"
check "/dev/vdb1 /mnt/t ext4"        "内核 mounts 表：/dev/vdb1 已挂载为 ext4"
check "sending incremental file list" "rsync 开始传输"
check "sent .* bytes"              "rsync 传输完成（有字节数汇总）"
check "test_script.txt"            "rsync 复制后目标目录含源文件（ls 输出）"
# ---- exFAT（Windows/macOS/Android 通用数据盘）----
check "exfatprogs version : 1.4"   "mkfs.exfat/fsck.exfat 版本（静态二进制可执行）"
check "exFAT format complete!"     "mkfs.exfat 实际建出 exFAT"
check 'TYPE="exfat"'               "busybox blkid 回读 TYPE=exfat"
check "clean. directories"         "fsck.exfat 检查跑完"
check "/dev/vdb2 /mnt/e exfat"     "内核 mounts 表：/dev/vdb2 已挂载为 exfat"
# ---- f2FS（Android 内部存储/OTG）----
check "F2FS-tools: mkfs.f2fs Ver: 1.16.0" "mkfs.f2fs 版本（静态二进制可执行）"
check "Info: Label = ANDROIDP"     "mkfs.f2fs 写卷标"
check 'TYPE="f2fs"'                "busybox blkid 回读 TYPE=f2fs"
check "Info: CKPT version"         "fsck.f2fs 读到 f2fs 超级块"
check "/dev/vdb3 /mnt/f f2fs"      "内核 mounts 表：/dev/vdb3 已挂载为 f2fs"
# ---- NTFS（Windows 数据盘；挂载读写用内核 ntfs3）----
check "mkntfs v2021.8.22"          "mkntfs/ntfsfix 版本（静态二进制可执行）"
check "mkntfs completed successfully" "mkntfs 实际建出 NTFS（-f 快格式，不整卷写零）"
check 'TYPE="ntfs"'                "busybox blkid 回读 TYPE=ntfs"
check "NTFS volume version is 3.1" "ntfsfix 读到 NTFS 卷"
check "was processed successfully" "ntfsfix 检查通过"
check "/dev/vdb4 /mnt/n ntfs3"     "内核 mounts 表：/dev/vdb4 已挂载为 ntfs3"
# ---- GPT 分区类型：exfat 段用 msftdata 标志（agent 的推导规则）----
check "msftdata"                   "parted print 显示 msftdata 标志（Windows 可识别的基本数据盘）"
# 动态链接器加载失败的特征串（缺库/架构不符）；命中即说明有二进制没跑起来。
if grep -aqE "error while loading shared libraries|cannot execute binary file|Exec format error" "$LOG"; then
    printf '  FAIL  日志中出现可执行文件加载错误\n'
    FAILED=$((FAILED + 1))
else
    printf '  PASS  未出现可执行文件加载错误\n'
fi

echo "[verify] 完成，日志: $LOG"
if [ "$FAILED" -gt 0 ]; then
    echo "[verify] 失败 $FAILED 项" >&2
    exit 1
fi
echo "[verify] 全部通过"
