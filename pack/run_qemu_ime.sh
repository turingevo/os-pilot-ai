#!/bin/sh
# 本地屏内置输入法端到端测试（QEMU 直启 AI 环境，VGA 本地控制台自绘屏 + 拼音输入法）
#
# 流程：
#   1. 起 mock LLM（18800，请求体落盘到 requests.log；测试盘 ai.json 指向 10.0.2.2:18800）
#   2. 起 QEMU：本地 VGA 作为 console（无 console=ttyS0），monitor 挂 unix socket
#   3. 用 sendkey 模拟键盘：Ctrl-Space 切输入法 → 拼音 → 数字选字 → 空格提交 → 回车送行
#   4. screendump 抓帧到截图目录；最后校验发给模型的中文并正常退出（exit → 关机）
#
# 用法: run_qemu_ime.sh [截图目录]
# 环境变量:
#   VTOY_AI_BUILD_DIR  构建目录（默认见 defaults.sh）
#   VTOY_IME_BOOT_WAIT 等待 guest 启动到提示符的秒数（默认 22）
#   VTOY_IME_KEEP      1 = 保留截图目录（默认保留；0 不删除，仅提示路径）
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
KERNEL="$BUILD/kernel/vmlinuz"
INITRD="$BUILD/initrd-ai.cpio.gz"
DISK="$BUILD/test/payload.img"
SHOTS="${1:-$BUILD/test/ime-shots}"
BOOT_WAIT="${VTOY_IME_BOOT_WAIT:-22}"
MON="$SHOTS/monitor.sock"
AI_DIR="$(cd "$(dirname "$0")/.." && pwd)"

for f in "$KERNEL" "$INITRD" "$DISK"; do
    [ -f "$f" ] || { echo "[ime-test] 缺少 $f" >&2; exit 1; }
done
command -v socat >/dev/null || { echo "[ime-test] 需要 socat（apt install socat）" >&2; exit 1; }
command -v qemu-system-x86_64 >/dev/null || { echo "[ime-test] 需要 qemu-system-x86_64" >&2; exit 1; }

rm -rf "$SHOTS"
mkdir -p "$SHOTS"
rm -f "$SHOTS/requests.log"

MOCK_PID=""
QEMU_PID=""
cleanup() {
    [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
    [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "[ime-test] 启动 mock LLM (18800)..."
VTOY_MOCK_LOG="$SHOTS/requests.log" nohup python3 "$AI_DIR/tools/mock_openai_server.py" 18800 \
    > "$SHOTS/mock.log" 2>&1 &
MOCK_PID=$!
sleep 1

echo "[ime-test] 启动 QEMU（本地 VGA 控制台，monitor: $MON）..."
# shellcheck disable=SC2086
qemu-system-x86_64 $VTOY_AI_QEMU_ACCEL \
    -m "$VTOY_AI_QEMU_MEM" -smp "$VTOY_AI_QEMU_SMP" \
    -kernel "$KERNEL" -initrd "$INITRD" \
    -append "rdinit=/init vtoy_ai=1 vtoy_ai_interactive=1 loglevel=3 console=tty0" \
    -drive file="$DISK",format=raw,if=virtio \
    -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
    -display none -monitor "unix:$MON,server,nowait" -serial none -no-reboot \
    > "$SHOTS/qemu.log" 2>&1 &
QEMU_PID=$!

mon() {
    printf '%s\n' "$1" | socat - UNIX-CONNECT:"$MON" >/dev/null 2>&1 || return 1
    sleep "${2:-0.4}"
}
shot() { mon "screendump $SHOTS/$1.ppm" 0.6; }
type_keys() { for k in $1; do mon "sendkey $k" 0.3; done; }

echo "[ime-test] 等待 guest 启动（${BOOT_WAIT}s）..."
sleep "$BOOT_WAIT"
shot 01-boot

echo "[ime-test] Ctrl-Space 开启输入法..."
mon "sendkey ctrl-spc"
shot 02-ime-on

echo "[ime-test] 输入拼音 bangwozhuangxitong..."
type_keys "b a n g w o z h u a n g x i t o n g"
shot 03-candidates

echo "[ime-test] 空格提交首选词..."
mon "sendkey spc"
shot 04-committed

echo "[ime-test] 回车送行（等待模型回复）..."
mon "sendkey ret"
sleep 6
shot 05-reply

echo "[ime-test] Ctrl-Space 关闭输入法，输入 exit 退出..."
mon "sendkey ctrl-spc"
type_keys "e x i t"
mon "sendkey ret"
n=0
while kill -0 "$QEMU_PID" 2>/dev/null && [ "$n" -lt 20 ]; do
    sleep 1
    n=$((n + 1))
done

fail=0
if kill -0 "$QEMU_PID" 2>/dev/null; then
    echo "[ime-test] 失败: exit 后 guest 未在 20s 内关机退出" >&2
    fail=1
else
    echo "[ime-test] guest 已正常关机退出"
    QEMU_PID=""
fi
if grep -q "帮我装系统" "$SHOTS/requests.log" 2>/dev/null; then
    echo "[ime-test] 通过: 发给模型的请求体包含中文「帮我装系统」"
else
    echo "[ime-test] 失败: 请求体中未找到中文输入（$SHOTS/requests.log）" >&2
    fail=1
fi

if command -v python3 >/dev/null && python3 -c "import PIL" 2>/dev/null; then
    for p in "$SHOTS"/*.ppm; do
        [ -e "$p" ] || continue
        python3 -c "
from PIL import Image
Image.open('$p').save('${p%.ppm}.png')
"
    done
    echo "[ime-test] 截图（PNG）: $SHOTS/01-boot.png、02-ime-on、03-candidates、04-committed、05-reply"
fi

echo "[ime-test] 产物: $SHOTS（截图、requests.log、mock.log、qemu.log）"
exit "$fail"
