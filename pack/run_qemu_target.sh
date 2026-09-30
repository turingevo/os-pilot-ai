#!/bin/sh
# 在 QEMU（UEFI/OVMF）里启动一块**已安装**的目标盘镜像，确认「装完能不能启动」。
#
# 与 run_qemu_usb.sh 的区别：那个挂真实 U 盘、要 root；这个只挂镜像文件，**不需要 root**，
# 用来在装完之后单独引导被装的那块盘。
#
# 用法: run_qemu_target.sh [磁盘镜像] [超时秒数]
# 环境变量:
#   VTOY_AI_TARGET_IMG   目标盘镜像（默认 $BUILD/test/reinstall-target.img）
#   VTOY_AI_TARGET_MODE  gui|tty（默认 gui：窗口里看真实启动过程；tty：串口接当前终端）
#   VTOY_AI_TARGET_LOG   串口日志（默认 $BUILD/test/qemu-target-serial.log）
#   VTOY_AI_TARGET_IF    磁盘接口 virtio|ide（默认 virtio）
#
# gui 默认不限时（关窗口或 Ctrl-C 结束）；tty 默认 300s 兜底。
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
DISK="${1:-${VTOY_AI_TARGET_IMG:-$BUILD/test/reinstall-target.img}}"
MODE="${VTOY_AI_TARGET_MODE:-gui}"
if [ "$MODE" = "gui" ]; then DEFAULT_TIMEOUT=0; else DEFAULT_TIMEOUT=300; fi
TIMEOUT="${2:-${VTOY_AI_QEMU_TIMEOUT:-$DEFAULT_TIMEOUT}}"
LOG="${VTOY_AI_TARGET_LOG:-$BUILD/test/qemu-target-serial.log}"
DISK_IF="${VTOY_AI_TARGET_IF:-virtio}"

OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd
OVMF_VARS_SRC=/usr/share/OVMF/OVMF_VARS_4M.fd
OVMF_VARS="$BUILD/test/OVMF_VARS_target.fd"

[ -f "$DISK" ] || { echo "错误: 找不到目标盘镜像 $DISK" >&2; exit 1; }
[ -f "$OVMF_CODE" ] || { echo "错误: 找不到 $OVMF_CODE（apt install ovmf）" >&2; exit 1; }
command -v qemu-system-x86_64 >/dev/null || { echo "错误: 缺少 qemu-system-x86_64" >&2; exit 1; }

cat >&2 <<EOF
[qemu-target] 目标盘: $DISK
[qemu-target] 提示: 若本机已用 udisksctl 挂载了该镜像，请先卸载——宿主与 guest 同时写会互相覆盖。
[qemu-target] 串口日志: $LOG
EOF

# 每次运行用干净的 UEFI 变量区，保证启动顺序不受上次影响（默认按镜像里的 ESP 引导）
cp "$OVMF_VARS_SRC" "$OVMF_VARS"
case "$DISK_IF" in
    virtio) DRIVE_ARGS="-drive file=$DISK,format=raw,if=virtio" ;;
    ide)    DRIVE_ARGS="-drive file=$DISK,format=raw,if=ide" ;;
    *) echo "错误: VTOY_AI_TARGET_IF 只能是 virtio 或 ide" >&2; exit 1 ;;
esac
case "$MODE" in
    gui) DISP_ARGS="-display gtk -vga virtio -serial file:$LOG" ;;
    tty) DISP_ARGS="-display none -serial stdio" ;;
    *) echo "错误: VTOY_AI_TARGET_MODE 只能是 gui 或 tty" >&2; exit 1 ;;
esac

if [ "$TIMEOUT" = "0" ]; then
    echo "[qemu-target] 模式 $MODE；超时 不限时（关窗口或 Ctrl-C 结束）"
else
    echo "[qemu-target] 模式 $MODE；超时 ${TIMEOUT}s"
fi

: > "$LOG"
# shellcheck disable=SC2086
timeout --foreground "$TIMEOUT" qemu-system-x86_64 \
    $VTOY_AI_QEMU_ACCEL \
    -m "$VTOY_AI_QEMU_MEM" -smp "$VTOY_AI_QEMU_SMP" \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$OVMF_VARS" \
    $DRIVE_ARGS \
    -netdev user,id=n0 \
    -device virtio-net-pci,netdev=n0 \
    $DISP_ARGS -monitor none -no-reboot \
    || true

echo "[qemu-target] 运行结束，日志尾部:"
tail -30 "$LOG" 2>/dev/null || true
