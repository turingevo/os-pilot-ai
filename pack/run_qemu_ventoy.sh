#!/bin/sh
# 在 QEMU（UEFI/OVMF）中启动 Ventoy 测试盘镜像，走真实 Ventoy 菜单（serial_console 主题）。
# 串口输出写入日志文件，供事后比对（T1 端到端验证：第一次启动 AI 助手，第二次启动被选中的镜像）。
#
# 前提: 宿主已启动 mock LLM 服务（测试盘里的 ai.json 指向 10.0.2.2:18800）:
#   VTOY_MOCK_CMD="cat /proc/cmdline" python3 tools/mock_openai_server.py 18800
#
# 用法: run_qemu_ventoy.sh [磁盘镜像] [串口日志] [超时秒数]
# 环境变量:
#   VTOY_AI_DISK         测试盘镜像（默认 $BUILD/test/ventoy-testdisk.img）
#   VTOY_AI_SERIAL_LOG   串口日志路径（默认 $BUILD/test/qemu-ventoy-serial.log）
#   VTOY_AI_QEMU_TIMEOUT 超时秒数（默认 240；guest 结束会自行关机，超时只是兜底）
#   VTOY_AI_DISK_IF      磁盘接口 virtio|ide（默认 virtio）
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
DISK="${1:-${VTOY_AI_DISK:-$BUILD/test/ventoy-testdisk.img}}"
LOG="${2:-${VTOY_AI_SERIAL_LOG:-$BUILD/test/qemu-ventoy-serial.log}}"
TIMEOUT="${3:-${VTOY_AI_QEMU_TIMEOUT:-240}}"
DISK_IF="${VTOY_AI_DISK_IF:-virtio}"

OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd
OVMF_VARS_SRC=/usr/share/OVMF/OVMF_VARS_4M.fd
OVMF_VARS="$BUILD/test/OVMF_VARS_ventoy.fd"

[ -f "$DISK" ] || { echo "错误: 找不到磁盘镜像 $DISK（先运行 make_ventoy_testdisk.sh）" >&2; exit 1; }
[ -f "$OVMF_CODE" ] || { echo "错误: 找不到 $OVMF_CODE（apt install ovmf）" >&2; exit 1; }
command -v qemu-system-x86_64 >/dev/null || { echo "错误: 缺少 qemu-system-x86_64" >&2; exit 1; }

# 每次运行用干净的 UEFI 变量区，保证启动顺序不受上次运行影响
cp "$OVMF_VARS_SRC" "$OVMF_VARS"
case "$DISK_IF" in
    virtio) DRIVE_ARGS="-drive file=$DISK,format=raw,if=virtio" ;;
    ide)    DRIVE_ARGS="-drive file=$DISK,format=raw,if=ide" ;;
    *) echo "错误: VTOY_AI_DISK_IF 只能是 virtio 或 ide" >&2; exit 1 ;;
esac

echo "[qemu-ventoy] 磁盘: $DISK (接口 $DISK_IF)"
echo "[qemu-ventoy] 串口日志: $LOG（每次运行覆盖）"
echo "[qemu-ventoy] 超时: ${TIMEOUT}s"
echo "[qemu-ventoy] 提醒: mock LLM 需监听宿主 18800（guest 通过 10.0.2.2 访问）"

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
    -display none -serial file:"$LOG" -monitor none -no-reboot \
    || true

echo "[qemu-ventoy] 运行结束，日志尾部:"
tail -40 "$LOG"
