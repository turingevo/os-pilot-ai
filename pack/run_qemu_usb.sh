#!/bin/sh
# 在 QEMU（UEFI/OVMF）里启动**真实 U 盘**（Ventoy 盘）做端到端测试。
#
# 与 run_qemu_ventoy.sh 的区别：那个跑的是镜像文件，这个直接挂真实块设备。
#   - 整盘给出（Ventoy 引导依赖 part2 的 EFI 引导器，不能只给分区）；
#   - 需要 root：/dev/sdX 默认 root:disk，普通用户无权打开；块设备也不吃 usb 直通以外的授权；
#   - 运行前必须卸载该盘全部分区（宿主与 guest 同时写会互相覆盖）。
#
# 用法: sudo -E sh run_qemu_usb.sh /dev/sdX [超时秒数]   # 0 = 不限时
# 环境变量:
#   VTOY_AI_USB_IF   virtio|usb  磁盘接口（默认 virtio；usb = qemu-xhci + usb-storage，走 USB 栈）
#   VTOY_AI_USB_MODE gui|tty     显示方式（默认 gui：窗口交互；tty：无显示，串口走当前终端）
#   VTOY_AI_USB_LOG  串口日志（默认 /tmp/qemu-usb-serial.log；tty 模式直接打到终端）
#   VTOY_AI_USB_YES=1            跳过设备确认
#   VTOY_AI_SCRATCH_MB >0        额外挂一块空白靶盘（受守卫磁盘工具的操作对象），单位 MiB；
#                                镜像不存在则自动创建，已存在则复用（不重置）
#   VTOY_AI_SCRATCH_IMG          靶盘镜像路径（默认 $BUILD/test/scratch-disk.img）
#                                **给了它就会挂靶盘**（无需再给 MB；镜像不存在时才需要 MB 指定大小）
#   VTOY_AI_SCRATCH_IF           靶盘接口 virtio|ide|usb（默认 virtio）
#   VTOY_AI_SCRATCH_SERIAL       靶盘序列号（默认 aitarget）；guest 内见 /sys/block/<dev>/serial，
#                                autoinstall 模板按它选盘。注意 virtio 用 -device 会改变盘序
set -e

. "$(dirname "$0")/defaults.sh"

DEV="${1:-}"
IFACE="${VTOY_AI_USB_IF:-virtio}"
MODE="${VTOY_AI_USB_MODE:-gui}"
# gui 是人机对话，从 QEMU 启动起算的固定倒计时必然在思考/输入途中把 guest 掐掉，
# 故默认不限时（关窗口或 Ctrl-C 结束）；tty/无人值守仍留 300s 兜底。
if [ "$MODE" = "gui" ]; then DEFAULT_TIMEOUT=0; else DEFAULT_TIMEOUT=300; fi
TIMEOUT="${2:-${VTOY_AI_QEMU_TIMEOUT:-$DEFAULT_TIMEOUT}}"
LOG="${VTOY_AI_USB_LOG:-/tmp/qemu-usb-serial.log}"

OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd
OVMF_VARS_SRC=/usr/share/OVMF/OVMF_VARS_4M.fd
OVMF_VARS=/tmp/OVMF_VARS_usb.fd

[ -n "$DEV" ] || { echo "用法: sudo -E sh $0 /dev/sdX [超时秒数]" >&2; exit 1; }
[ -b "$DEV" ] || { echo "错误: $DEV 不是块设备（Ventoy 盘要整盘给出，如 /dev/sdd）" >&2; exit 1; }
BASE=$(basename "$DEV")

# ---- 安全闸：只接受 USB 盘，且必须未挂载 ----
TRAN=$(lsblk -ndo TRAN "/dev/$BASE" 2>/dev/null | tr -d ' ')
[ "$TRAN" = "usb" ] || {
    echo "拒绝: /dev/$BASE 传输类型为 '${TRAN:-未知}'，不是 USB 盘（防止误选系统盘）" >&2
    exit 1
}
if lsblk -nlo MOUNTPOINTS "/dev/$BASE" | grep -q .; then
    echo "拒绝: /dev/$BASE 仍有分区挂载中，先卸载再运行：" >&2
    lsblk -no NAME,LABEL,MOUNTPOINTS "/dev/$BASE" >&2
    echo "  udisksctl unmount -b /dev/${BASE}1" >&2
    echo "  udisksctl unmount -b /dev/${BASE}2" >&2
    exit 1
fi

echo "[qemu-usb] 目标设备:"
lsblk -o NAME,SIZE,FSTYPE,LABEL,MODEL "/dev/$BASE"
if [ "${VTOY_AI_USB_YES:-}" != "1" ]; then
    printf "[qemu-usb] guest 会读写该设备（agent 会改 ventoy.json、写日志）。继续? [y/N] "
    read ans || ans=""
    case "$ans" in y|Y|yes|YES) ;; *) echo "已取消"; exit 1 ;; esac
fi

[ -w "$DEV" ] || { echo "错误: 当前用户无权打开 $DEV，请用 sudo -E 运行本脚本" >&2; exit 1; }
[ -f "$OVMF_CODE" ] || { echo "错误: 缺少 $OVMF_CODE（apt install ovmf）" >&2; exit 1; }
command -v qemu-system-x86_64 >/dev/null || { echo "错误: 缺少 qemu-system-x86_64" >&2; exit 1; }

# 每次运行用干净的 UEFI 变量区，保证启动顺序不受上次影响
cp "$OVMF_VARS_SRC" "$OVMF_VARS"

case "$IFACE" in
    virtio) DRIVE_ARGS="-drive file=$DEV,format=raw,if=virtio" ;;
    usb)    DRIVE_ARGS="-drive file=$DEV,format=raw,if=none,id=usbstick -device qemu-xhci,id=xhci -device usb-storage,drive=usbstick,bus=xhci.0" ;;
    *) echo "错误: VTOY_AI_USB_IF 只能是 virtio 或 usb" >&2; exit 1 ;;
esac

# ---- 可选：额外挂一块空白靶盘，供受守卫磁盘工具分区/格式化/备份 ----
# 与主盘（真实 U 盘，承载 payload、受硬守卫保护）分开：靶盘无任何数据，可随意写。
SCRATCH_IMG="${VTOY_AI_SCRATCH_IMG:-$VTOY_AI_BUILD_DIR/test/scratch-disk.img}"
SCRATCH_IF="${VTOY_AI_SCRATCH_IF:-virtio}"
# 靶盘序列号：guest 内出现在 /sys/block/<dev>/serial；autoinstall 模板按它选盘（不依赖 /dev/vdX）。
SCRATCH_SERIAL="${VTOY_AI_SCRATCH_SERIAL:-aitarget}"
EXTRA_ARGS=""
# 是否挂靶盘：**显式给了镜像路径**、或给了非 0 的 MB，都算"要靶盘"。
# 只认 MB 会踩坑：想指定路径却忘了 MB，靶盘就悄悄不挂 —— guest 里只剩 U 盘一块盘。
SCRATCH_MB="${VTOY_AI_SCRATCH_MB:-}"
if [ -n "$VTOY_AI_SCRATCH_IMG" ] || { [ -n "$SCRATCH_MB" ] && [ "$SCRATCH_MB" != "0" ]; }; then
    case "$SCRATCH_MB" in
        '') SCRATCH_MB=0 ;;
        *[!0-9]*) echo "错误: VTOY_AI_SCRATCH_MB 必须是整数 MiB" >&2; exit 1 ;;
    esac
    mkdir -p "$(dirname "$SCRATCH_IMG")"
    if [ -f "$SCRATCH_IMG" ]; then
        echo "[qemu-usb] 复用已有靶盘: $SCRATCH_IMG（$(stat -c %s "$SCRATCH_IMG") 字节；要重置回空白请先删掉或用 make_blank_disk.sh 重建）"
    elif [ "$SCRATCH_MB" -gt 0 ]; then
        truncate -s "${SCRATCH_MB}M" "$SCRATCH_IMG"
        echo "[qemu-usb] 新建空白靶盘: $SCRATCH_IMG（${SCRATCH_MB}MiB，稀疏文件、无分区表）"
    else
        echo "错误: 靶盘镜像 $SCRATCH_IMG 不存在，且未给 VTOY_AI_SCRATCH_MB 指定首次创建大小" >&2
        echo "      先用 make_blank_disk.sh 造好，或补上 VTOY_AI_SCRATCH_MB=<MiB>" >&2
        exit 1
    fi
    case "$SCRATCH_IF" in
        # 用 -device 才能带 serial；注意这会改变盘序（靶盘可能成为 /dev/vda），
        # 所以引导后按 serial 识别靶盘，别认死 /dev/vdb。
        virtio) EXTRA_ARGS="-drive file=$SCRATCH_IMG,format=raw,if=none,id=scratch -device virtio-blk-pci,drive=scratch,serial=$SCRATCH_SERIAL" ;;
        ide)    EXTRA_ARGS="-drive file=$SCRATCH_IMG,format=raw,if=ide" ;;
        usb)    EXTRA_ARGS="-drive file=$SCRATCH_IMG,format=raw,if=none,id=scratch -device qemu-xhci,id=xhci2 -device usb-storage,drive=scratch,bus=xhci2.0" ;;
        *) echo "错误: VTOY_AI_SCRATCH_IF 只能是 virtio、ide 或 usb" >&2; exit 1 ;;
    esac
else
    echo "[qemu-usb] 注意: 未挂靶盘（guest 内只有 U 盘一块盘）。要挂就给 VTOY_AI_SCRATCH_IMG=<镜像>（或 VTOY_AI_SCRATCH_MB=<MiB>）"
fi
case "$MODE" in
    # -vga virtio: 规避默认 VGA 下 QEMU 6.2 GTK 窗口随机黑屏问题（窗口缩为 320x240、guest fb 退化为 8x1）
    gui) DISP_ARGS="-display gtk -vga virtio -serial file:$LOG" ;;
    tty) DISP_ARGS="-display none -serial stdio" ;;
    *) echo "错误: VTOY_AI_USB_MODE 只能是 gui 或 tty" >&2; exit 1 ;;
esac

if [ "$TIMEOUT" = "0" ]; then
    echo "[qemu-usb] 接口 $IFACE / 模式 $MODE；超时 不限时（agent 会跑多久都行；关窗口或 Ctrl-C 结束）"
else
    echo "[qemu-usb] 接口 $IFACE / 模式 $MODE；超时 ${TIMEOUT}s（到点 timeout 会给 QEMU 发 SIGTERM 强制结束）"
fi
if [ "$MODE" = "gui" ]; then
    echo "[qemu-usb] 提醒: 需带桌面环境运行（sudo -E 保留 DISPLAY/XAUTHORITY）；串口日志: $LOG"
else
    echo "[qemu-usb] 提醒: 串口直连本终端，AI 环境界面（串口顺序的内核命令行）就在这里交互"
fi
if [ -n "$EXTRA_ARGS" ]; then
    echo "[qemu-usb] 靶盘: $SCRATCH_IMG（接口 $SCRATCH_IF，序列号 $SCRATCH_SERIAL）"
    echo "[qemu-usb] 提示: 引导后用 list_disks 按序列号认靶盘（盘序可能变，别认死 /dev/vdb）"
fi

# shellcheck disable=SC2086
set +e
timeout --foreground "$TIMEOUT" qemu-system-x86_64 \
    $VTOY_AI_QEMU_ACCEL \
    -m "$VTOY_AI_QEMU_MEM" -smp "$VTOY_AI_QEMU_SMP" \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$OVMF_VARS" \
    $DRIVE_ARGS \
    $EXTRA_ARGS \
    -netdev user,id=n0 \
    -device virtio-net-pci,netdev=n0 \
    $DISP_ARGS -monitor none -no-reboot
rc=$?
set -e
if [ "$rc" = "124" ]; then
    echo "[qemu-usb] 到点 ${TIMEOUT}s 超时，QEMU 被 timeout 终止（非 guest 崩溃）" >&2
fi

echo "[qemu-usb] 运行结束"
