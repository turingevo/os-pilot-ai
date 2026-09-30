#!/bin/sh
# 在 QEMU 中启动 AI 环境（x86_64，串口控制台）
# 用法: run_qemu.sh [内核] [initrd] [payload 磁盘] [超时秒数]
# 环境变量:
#   VTOY_AI_BUILD_DIR    构建目录（默认见 defaults.sh；内核/initrd/磁盘都从这里取）
#   VTOY_AI_INTERACTIVE=1  串口接入当前终端，直接对话；强制交互（忽略 test_script.txt）；
#                          超时秒数默认 0 = 不限时
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
KERNEL="${1:-$BUILD/kernel/vmlinuz}"
INITRD="${2:-$BUILD/initrd-ai.cpio.gz}"
DISK="${3:-$BUILD/test/payload.img}"
INTERACTIVE="${VTOY_AI_INTERACTIVE:-0}"
DEFAULT_TIMEOUT=180
[ "$INTERACTIVE" = "1" ] && DEFAULT_TIMEOUT=0
TIMEOUT="${4:-$DEFAULT_TIMEOUT}"

LOG="$(dirname "$DISK")/qemu-serial.log"
APPEND="console=ttyS0 loglevel=3 rdinit=/init vtoy_ai=1"

# 交互模式用 -nographic（串口与监视器复用 stdio），并提供 Ctrl-A X 强制退出。
# 注意：QEMU 会把 stdio 终端切到 raw 模式（tcsetattr），这要求它处于前台进程组，
# 否则会被内核用 SIGTTOU 停住（表现为启动后无任何输出，见下方 timeout --foreground）。
if [ "$INTERACTIVE" = "1" ]; then
    IO_ARGS="-nographic"
    APPEND="$APPEND vtoy_ai_interactive=1"
    echo "[qemu] 交互模式：约 15-20 秒后出现「你> 」提示，直接输入"
    echo "[qemu] 输入 exit 退出并自动关机；强制结束按 Ctrl-A 再按 X"
    echo "[qemu] 会话记录写在 guest 镜像的 /ventoy/ai/logs/ 下（无串口日志文件）"
else
    IO_ARGS="-display none -serial file:$LOG -monitor none"
fi

echo "[qemu] kernel=$KERNEL"
echo "[qemu] initrd=$INITRD"
echo "[qemu] disk=$DISK (timeout ${TIMEOUT}s)"

# shellcheck disable=SC2086
# --foreground：不让 timeout 为子进程另建进程组（否则 QEMU 处于后台进程组，
# 切终端 raw 模式时被 SIGTTOU 停住，终端里表现为卡死无输出）。
timeout --foreground "$TIMEOUT" qemu-system-x86_64 \
    $VTOY_AI_QEMU_ACCEL \
    -m "$VTOY_AI_QEMU_MEM" -smp "$VTOY_AI_QEMU_SMP" \
    -kernel "$KERNEL" \
    -initrd "$INITRD" \
    -append "$APPEND" \
    -drive file="$DISK",format=raw,if=virtio \
    -netdev user,id=n0 \
    -device virtio-net-pci,netdev=n0 \
    $IO_ARGS -no-reboot \
    || true

if [ "$INTERACTIVE" = "1" ]; then
    echo "[qemu] 运行结束"
else
    echo "[qemu] 运行结束，串口日志: $LOG"
    tail -60 "$LOG"
fi
