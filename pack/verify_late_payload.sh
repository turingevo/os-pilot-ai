#!/bin/sh
# 时序回归：载荷盘（U 盘）比内置盘**晚出现**时，AI 环境必须等到它、挂它，而不是挂内置盘。
#
# 为什么需要这个脚本：真机上 U 盘要等 init 里 modprobe usb-storage/uas + 一次 SCSI 扫描才
# 出现节点，而内置硬盘在 init 跑起来之前就有分区。QEMU 直接挂磁盘时两台设备同时可见，
# 这个先后差异永远碰不到 —— 上一版就是因此漏掉了"挂错盘/没盘可挂"的真机故障。
#
# 拓扑（与真机对齐）：
#   - 内置盘 vda：virtio 直挂，开机即在。分区里有 ventoy/ 目录但**没有** ventoy/ai 与
#     ventoy/ai.json —— 正是"另一支装过 Ventoy 的盘"的形态（弱标记）。
#   - U 盘 sda：USB 存储，默认**不**接上总线，延迟 ${DELAY}s 后经 monitor device_add 热插。
#
# 判据（全部满足才算过）：
#   1) 命中设备是 /dev/sd*，不是 /dev/vda
#   2) 数据分区确实挂到了 /iso
#   3) 串口日志点名的那份 boot-*.log，确实能从 U 盘镜像里取出来且非空
#      （按名字取件，不用比对目录清单：这个名字只可能由本次运行生成，模板里的旧日志不会干扰）
#   4) 内置盘分区文件列表与基线逐行一致（一个字节都没往错盘上写）
#
# 用法: verify_late_payload.sh [initrd] [热插延迟秒，默认 8]
# 前置: 先跑 pack/pack_env.sh + pack/make_test_disk.sh 产出 payload.img
# 环境变量: VTOY_AI_BUILD_DIR（见 defaults.sh）
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
INITRD="${1:-$BUILD/initrd-ai.cpio.gz}"
DELAY="${2:-8}"
WORK="$BUILD/test/late-payload"
STICK_TPL="$BUILD/test/payload.img"
SERIAL="$WORK/serial.log"
MON="$WORK/mon.sock"

[ -f "$INITRD" ] || { echo "错误: 找不到 initrd: $INITRD（先跑 pack/pack_env.sh）" >&2; exit 1; }
[ -f "$BUILD/kernel/vmlinuz" ] || { echo "错误: 找不到内核: $BUILD/kernel/vmlinuz（先跑 pack/fetch_kernel.sh）" >&2; exit 1; }
[ -f "$STICK_TPL" ] || { echo "错误: 找不到载荷模板 $STICK_TPL（先跑 pack/make_test_disk.sh）" >&2; exit 1; }
command -v debugfs >/dev/null 2>&1 || { echo "错误: 宿主缺少 debugfs（e2fsprogs）" >&2; exit 1; }

mkdir -p "$WORK"
rm -f "$SERIAL" "$MON"

# ---------- 造两台盘 ----------
# 每次 rm 重建：整盘镜像 offset 0 若残留上一轮的 MBR/超级块，mkfs 会报假校验错；诱饵盘也必须
# 回到只有 note.txt 的干净状态，"未被写入"这条判据才有意义。
echo "[late] 制作 U 盘镜像（载荷 = $STICK_TPL）"
rm -f "$WORK/stick.img" "$WORK/stick-p1.img"
truncate -s 81M "$WORK/stick.img"
printf 'label: dos\nstart=2048\n' | sfdisk --force "$WORK/stick.img" >/dev/null 2>&1
dd if="$STICK_TPL" of="$WORK/stick.img" bs=1M seek=1 conv=notrunc status=none

echo "[late] 制作内置盘镜像（只有弱标记 ventoy/，无 ventoy/ai）"
rm -f "$WORK/internal.img" "$WORK/decoy-fs.img" "$WORK/decoy-p1.img" "$WORK/d.txt"
mkdir -p "$WORK/decoy/ventoy"
printf 'this disk is not the AI payload disk\n' > "$WORK/decoy/ventoy/note.txt"
truncate -s 65M "$WORK/internal.img"
printf 'label: dos\nstart=2048\n' | sfdisk --force "$WORK/internal.img" >/dev/null 2>&1
truncate -s 64M "$WORK/decoy-fs.img"
mkfs.ext4 -q -F -d "$WORK/decoy" "$WORK/decoy-fs.img"
dd if="$WORK/decoy-fs.img" of="$WORK/internal.img" bs=1M seek=1 conv=notrunc status=none

# ---------- 基线清单 ----------
extract_part() {   # extract_part <整盘img> <目标分区img>
    dd if="$1" of="$2" bs=1M skip=1 count=80 status=none 2>/dev/null
}
ls_dir() {         # ls_dir <分区img> <目录> —— 逐行输出条目名，便于 diff
    # debugfs 的 `ls -d` 把整个目录打在同一行：`23 (12) .   24 (40) boot-x.log`。
    # 按空白拆开后丢掉 inode 号、记录长度、. / .. 以及已删除条目的占位符 `<24>`，剩下的就是条目名
    # （本用例文件名不含空格）。
    # 宿主 e2fsprogs 1.46.5 的 debugfs 没有 -r 选项，默认打开就是只读。
    debugfs -R "ls -d $2" "$1" 2>/dev/null |
        tr -s ' \t' '\n' |
        grep -Ev '^$|^[0-9]+$|^\([0-9]+\)$|^\.\.?$|^<[^>]+>$' |
        sort || true
}
extract_part "$WORK/internal.img" "$WORK/decoy-p1.img"
BASE_DECOY=$(ls_dir "$WORK/decoy-p1.img" /ventoy)

# ---------- 启动 + 延迟热插 ----------
python3 - "$MON" "$DELAY" <<'PY' &
import socket, sys, time
sock, delay = sys.argv[1], float(sys.argv[2])
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
for _ in range(400):
    try:
        s.connect(sock); break
    except OSError:
        time.sleep(0.05)
else:
    sys.exit("[late] monitor 连不上，无法热插 U 盘")
time.sleep(delay)
s.sendall(b"device_add usb-storage,drive=stick,bus=xhci.0\n")
time.sleep(1)
PY
HOTPLUG_PID=$!
trap 'kill $HOTPLUG_PID 2>/dev/null || true' EXIT

# shellcheck disable=SC2086
timeout --foreground 180 qemu-system-x86_64 \
    $VTOY_AI_QEMU_ACCEL \
    -m "$VTOY_AI_QEMU_MEM" -smp "$VTOY_AI_QEMU_SMP" \
    -kernel "$BUILD/kernel/vmlinuz" -initrd "$INITRD" \
    -append "console=ttyS0 loglevel=3 rdinit=/init vtoy_ai=1" \
    -device nec-usb-xhci,id=xhci \
    -drive if=none,id=stick,file="$WORK/stick.img",format=raw \
    -drive if=none,id=intl,file="$WORK/internal.img",format=raw \
    -device virtio-blk-pci,drive=intl \
    -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
    -display none -serial file:"$SERIAL" -monitor unix:"$MON",server,nowait -no-reboot \
    || true

kill $HOTPLUG_PID 2>/dev/null || true
trap - EXIT

# ---------- 判定 ----------
extract_part "$WORK/stick.img" "$WORK/stick-p1.img"
extract_part "$WORK/internal.img" "$WORK/decoy-p1.img"
NOW_DECOY=$(ls_dir "$WORK/decoy-p1.img" /ventoy)

fail=""
say() { printf '%s\n' "$*"; }
pass() { say "  [ok]   $*"; }
bad()  { say "  [FAIL] $*"; fail="${fail}  - $*\n"; }
has()  { grep -q "$1" "$2" 2>/dev/null; }

# 顶部有 set -e，所以判据一律写成 if：`grep -q ...; check $?` 会在 grep 失败时直接退出，
# 后面的判据根本不会跑。
# 两版输出格式都要认：新版「[mount_payload] 命中 /dev/sda1 (ext4, rw)」，修复前的旧版只打
# 「[mount_payload] /dev/vda (ext4, rw)」。只按新格式取会让旧 initrd 得到空值、报成"没挂上"，
# 而真相是"挂到了内置盘"——诊断误导比失败更难查。
HIT=$(grep -o '命中 /dev/[a-z0-9]*' "$SERIAL" | head -1 | sed 's#^命中 ##' || true)
[ -n "$HIT" ] || HIT=$(grep -o '\[mount_payload\] /dev/[a-z0-9]*' "$SERIAL" | head -1 |
                     sed 's#.*\] ##' || true)
say "[late] 串口日志: $SERIAL；命中设备: ${HIT:-（无）}"

if has '查找 Ventoy 数据分区（最多等' "$SERIAL"; then
    pass "init 进入了等待载荷分区的循环"
else
    bad "init 没有进入等待循环（缺少超时上界，早退）"
fi

case "$HIT" in
    /dev/sd*) pass "载荷盘命中在 USB 设备（$HIT）" ;;
    "")       bad "没有任何设备被命中：到超时都没挂上载荷分区" ;;
    *)        bad "载荷盘应命中 /dev/sd*，实际命中 $HIT —— 挂到别的盘上了" ;;
esac

if has '数据分区已挂载到 /iso' "$SERIAL"; then
    pass "/iso 挂载成功（挂的是哪块盘由上面 HIT 那条判定）"
else
    bad "/iso 没有挂上任何分区"
fi

if [ "$NOW_DECOY" = "$BASE_DECOY" ]; then
    pass "内置盘（弱标记盘）未被写入任何文件"
else
    bad "内置盘被写入：基线 [${BASE_DECOY}] 现在 [${NOW_DECOY}]"
fi

# 只认串口日志点名的这一个文件名：它由本次运行生成，不可能与模板里的旧日志重名，
# 因此不需要先清空 U 盘就能证明"证据真的落到了 U 盘上"。
LOGPATH=$(grep -o '本次启动日志: /iso/[^ ]*boot-[^ ]*\.log' "$SERIAL" | head -1 |
          sed 's#^本次启动日志: /iso/##' || true)
if [ -z "$LOGPATH" ]; then
    bad "串口日志里没有「本次启动日志:」这一行（数据分区不可写或根本没挂上）"
else
    rm -f "$WORK/boot.log"
    printf 'dump /%s %s\n' "$LOGPATH" "$WORK/boot.log" > "$WORK/d.txt"
    debugfs -f "$WORK/d.txt" "$WORK/stick-p1.img" >/dev/null 2>&1 || true
    if [ -s "$WORK/boot.log" ]; then
        pass "U 盘上有本次启动日志 $LOGPATH（$(wc -c < "$WORK/boot.log") 字节），关键行:"
        grep -E '分区探测|数据分区已挂载|本次启动日志|退出（rc|时钟:' "$WORK/boot.log" | sed 's/^/    /' || true
    else
        bad "串口说写了 $LOGPATH，但 U 盘镜像里取不出来（写到了别的盘？）"
    fi
fi
say "[late] agent 退出码行: $(grep -o '退出（rc=[0-9]*）' "$SERIAL" | head -1 || echo 无)"

if [ -n "$fail" ]; then
    printf '%b\n' "\n[late] 未通过:\n$fail" >&2
    exit 1
fi
say "[late] 通过：晚到的 U 盘被等到并挂载，证据落在 U 盘，弱标记盘未被污染"
