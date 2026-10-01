#!/bin/sh
# 生成 Ventoy 测试盘镜像（T1 端到端验证用）：把官方 Ventoy 引导资产与本项目自建的
# 数据分区组装成一个完整 U 盘镜像文件，供 QEMU 以 UEFI 方式启动、走真实的 Ventoy 菜单。
#
# 免 root：分区表用 sfdisk 直写镜像文件，数据分区用 mtools 填充内容。
#
# 磁盘布局（与官方安装器一致，MBR）:
#   sector 0            : boot.img 前 446 字节（引导代码）+ 分区表
#   sector 1..2047      : core.img（BIOS 引导用；UEFI 不读，但保持磁盘结构完整）
#   part1               : FAT32（卷标 Ventoy）= 数据分区，放 ISO 与 ventoy 配置
#   part2（32MiB）      : VTOYEFI，由官方 ventoy.disk.img.xz 解压写入
#   offset 0x180 / 0x1B8: 磁盘 UUID / 磁盘签名（随机值，与官方安装器写法一致）
#
# 用法: make_ventoy_testdisk.sh [输出镜像]
# 环境变量:
#   VTOY_AI_VENTOY_RELEASE  官方 Ventoy 发布包目录（含 boot/ 与 ventoy/）
#   VTOY_AI_BUILD_DIR       构建目录（默认见 defaults.sh）
#   VTOY_AI_DISK_MB         磁盘大小 MiB（默认 128，需能放下两个 mini-ISO）
#   VTOY_AI_SKIP_ISO_BUILD  置 1 则复用已存在的 mini-ISO，不重新生成
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
OUT="${1:-$BUILD/test/ventoy-testdisk.img}"
DISK_MB="${VTOY_AI_DISK_MB:-128}"
PART2_SECTORS=65536
PART1_START=2048

PACK_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$PACK_DIR/find_mtools.sh"

# ---------- 定位官方 Ventoy 发布包（提供 boot/ 与 ventoy/ 引导资产） ----------
REL="${VTOY_AI_VENTOY_RELEASE:-}"
if [ -z "$REL" ]; then
    # 未显式指定时，在构建目录与常见下载目录里找解压好的官方发布包
    for d in "$BUILD"/ventoy-*/ "$BUILD"/dl/ventoy-*/ "$HOME"/Downloads/ventoy-*/; do
        if [ -f "$d/boot/core.img.xz" ] && [ -f "$d/ventoy/ventoy.disk.img.xz" ]; then
            REL="$d"
            break
        fi
    done
fi
if [ -z "$REL" ] || [ ! -f "$REL/boot/core.img.xz" ] || [ ! -f "$REL/ventoy/ventoy.disk.img.xz" ]; then
    echo "错误: 未找到官方 Ventoy 发布包（需要 boot/boot.img、boot/core.img.xz、ventoy/ventoy.disk.img.xz）" >&2
    echo "  从官网下载发布包并解压，然后二选一指定其目录：" >&2
    echo "    VTOY_AI_VENTOY_RELEASE=<目录> sh $0" >&2
    echo "    或写进 pack/defaults.user.sh（本机私有，不进仓库）" >&2
    exit 1
fi

for t in sfdisk mkfs.vfat xzcat; do
    command -v $t >/dev/null || { echo "错误: 缺少工具 $t" >&2; exit 1; }
done

WORK="$BUILD/test/ventoy-staging"
rm -rf "$WORK"
mkdir -p "$WORK"

# ---------- 生成两个 mini-ISO ----------
# 0-OS-PILOT-AI.iso：T1 入口本体，Ventoy 菜单里的“AI 助手”条目
# ubuntu-24.04-desktop-amd64.iso：同一环境的副本，但内核命令行多一个标记
#   （vtoy_ai_tag=ubuntu-copy）。两次启动对比 /proc/cmdline 即可证明 Ventoy
#   第二次确实按 agent 写入的 VTOY_DEFAULT_IMAGE 选择了这个文件。
# 注意用 td- 前缀区分：生产 ISO（$BUILD/test/0-OS-PILOT-AI.iso，真机取向）不能被测试盘
# 覆盖成串口优先版本，否则手工拷盘时又会“真机黑屏”。
ISO_BASE="$BUILD/test/td-0-OS-PILOT-AI.iso"
ISO_UBUNTU="$BUILD/test/td-ubuntu-24.04-desktop-amd64.iso"
if [ "${VTOY_AI_SKIP_ISO_BUILD:-0}" = "1" ]; then
    for f in "$ISO_BASE" "$ISO_UBUNTU"; do
        [ -f "$f" ] || { echo "错误: 缺少 $f（去掉 VTOY_AI_SKIP_ISO_BUILD 重新生成）" >&2; exit 1; }
    done
else
    # 测试盘供 QEMU 无头运行（-serial file:）使用：串口优先 ⇒ 界面与全量用户态输出都落在
    # 串口日志里，便于断言；make_iso.sh 的默认值已改为真机取向（显示器优先），这里显式覆盖。
    TEST_CMDLINE="console=tty0 console=ttyS0,115200 loglevel=3 rdinit=/init vtoy_ai=1"
    VTOY_AI_CMDLINE="$TEST_CMDLINE" sh "$PACK_DIR/make_iso.sh" "$ISO_BASE"
    VTOY_AI_CMDLINE="$TEST_CMDLINE" VTOY_AI_CMDLINE_EXTRA="vtoy_ai_tag=ubuntu-copy" sh "$PACK_DIR/make_iso.sh" "$ISO_UBUNTU"
fi
[ -f "$ISO_UBUNTU" ] || { echo "错误: 缺少 $ISO_UBUNTU" >&2; exit 1; }

# 占位 ISO：让菜单出现第三方镜像，也更接近真实 U 盘
FAKE_ISO="$WORK/debian-12-netinst-amd64.iso"
dd if=/dev/zero of="$FAKE_ISO" bs=1024 count=64 status=none

# ---------- 数据分区内容 ----------
mkdir -p "$WORK/part1/ventoy/ai" "$WORK/part1/scripts"
# VTOY_SECONDARY_BOOT_MENU=0：关掉 Ventoy 1.1.05 默认开启的“二级动作菜单”
# （正常模式/grub2/memdisk/校验和）。该菜单默认无倒计时，会把无人值守启动卡住。
cat > "$WORK/part1/ventoy/ventoy.json" <<'EOF'
{
    "theme": {
        "display_mode": "serial_console",
        "serial_param": "--unit=0 --speed=115200 --word=8 --parity=no --stop=1"
    },
    "control": [
        { "VTOY_MENU_TIMEOUT": "10" },
        { "VTOY_DEFAULT_IMAGE": "/0-OS-PILOT-AI.iso" },
        { "VTOY_SECONDARY_BOOT_MENU": "0" }
    ]
}
EOF
cat > "$WORK/part1/ventoy/ai.json" <<'EOF'
{
    "provider": "openai-compatible",
    "base_url": "http://10.0.2.2:18800/v1",
    "model": "mock-model",
    "api_key": "",
    "temperature": 0.2,
    "max_tokens": 262144,
    "language": "zh-CN",
    "mode": "orchestrate",
    "request_timeout": 30,
    "payload_dir": "/iso",
    "log_dir": "/iso/ventoy/ai/logs"
}
EOF
cat > "$WORK/part1/ventoy/ai/test_script.txt" <<'EOF'
你好，请先看看U盘里有哪些镜像
帮我给 ubuntu-24.04-desktop-amd64.iso 配置自动安装，用 /scripts/ubuntu.seed
确认写入配置
再帮我检查一下磁盘环境
EOF
cat > "$WORK/part1/scripts/ubuntu.seed" <<'EOF'
#cloud-config
autoinstall:
  version: 1
  identity:
    hostname: ventoy-ai
    username: ventoy
    password: "$6$rounds=4096$ventoy$2b2b2b2b"
  storage:
    layout:
      name: direct
EOF

# ---------- 计算分区布局（与 VentoyWorker.sh 的 MBR 计算一致） ----------
TOTAL_SECTORS=$((DISK_MB * 2048))
PART1_END=$((TOTAL_SECTORS - PART2_SECTORS - 1))
PART2_START=$((PART1_END + 1))
MOD=$((PART2_START % 8))
if [ $MOD -gt 0 ]; then
    PART1_END=$((PART1_END - MOD))
    PART2_START=$((PART1_END + 1))
fi
PART2_END=$((PART2_START + PART2_SECTORS - 1))
PART1_SECTORS=$((PART1_END - PART1_START + 1))
PART1_BYTES=$((PART1_SECTORS * 512))

NEED_BYTES=$(( $(stat -c %s "$ISO_BASE") + $(stat -c %s "$ISO_UBUNTU") + 1048576 ))
if [ "$PART1_BYTES" -lt "$NEED_BYTES" ]; then
    echo "错误: 磁盘太小，数据分区 $((PART1_BYTES / 1048576))MiB < 需要 $((NEED_BYTES / 1048576))MiB，请调大 VTOY_AI_DISK_MB" >&2
    exit 1
fi

echo "[testdisk] 磁盘 ${DISK_MB}MiB: part1 ${PART1_START}..${PART1_END} (type 07, $((PART1_BYTES / 1048576))MiB), part2 ${PART2_START}..${PART2_END} (type EF, 32MiB)"

# ---------- 分区表 + 引导代码 ----------
rm -f "$OUT"
truncate -s $((TOTAL_SECTORS * 512)) "$OUT"
cat > "$WORK/part.sfdisk" <<EOF
label: dos
unit: sectors

start=$PART1_START, size=$PART1_SECTORS, type=7, bootable
start=$PART2_START, size=$PART2_SECTORS, type=ef
EOF
sfdisk -q "$OUT" < "$WORK/part.sfdisk"

dd if="$REL/boot/boot.img" of="$OUT" bs=1 count=446 conv=notrunc status=none
xzcat "$REL/boot/core.img.xz" | dd of="$OUT" bs=512 count=2047 seek=1 conv=notrunc status=none
echo "[testdisk] 写入 VTOYEFI 分区（32MiB）..."
xzcat "$REL/ventoy/ventoy.disk.img.xz" | dd of="$OUT" bs=512 count=$PART2_SECTORS seek=$PART2_START conv=notrunc status=none

# 磁盘 UUID / 磁盘签名（官方安装器用随机 UUID 填充这两处）
head -c 16 /dev/urandom | dd of="$OUT" bs=1 seek=384 conv=notrunc status=none
head -c 16 /dev/urandom | dd of="$OUT" bs=1 skip=12 seek=440 count=4 conv=notrunc status=none

# ---------- 数据分区（FAT32）----------
PART1_IMG="$WORK/part1.img"
truncate -s "$PART1_BYTES" "$PART1_IMG"
mkfs.vfat -F 32 -n Ventoy "$PART1_IMG" >/dev/null
"$MMD" -i "$PART1_IMG" ::/ventoy ::/ventoy/ai ::/scripts
"$MCOPY" -i "$PART1_IMG" "$ISO_BASE" ::/0-OS-PILOT-AI.iso
"$MCOPY" -i "$PART1_IMG" "$ISO_UBUNTU" ::/ubuntu-24.04-desktop-amd64.iso
"$MCOPY" -i "$PART1_IMG" "$FAKE_ISO" ::/debian-12-netinst-amd64.iso
"$MCOPY" -i "$PART1_IMG" "$WORK/part1/ventoy/ventoy.json" ::/ventoy/ventoy.json
"$MCOPY" -i "$PART1_IMG" "$WORK/part1/ventoy/ai.json" ::/ventoy/ai.json
"$MCOPY" -i "$PART1_IMG" "$WORK/part1/ventoy/ai/test_script.txt" ::/ventoy/ai/test_script.txt
"$MCOPY" -i "$PART1_IMG" "$WORK/part1/scripts/ubuntu.seed" ::/scripts/ubuntu.seed
dd if="$PART1_IMG" of="$OUT" bs=512 seek=$PART1_START conv=notrunc status=none

sync
echo "[testdisk] 完成: $OUT ($(stat -c %s "$OUT") 字节)"
echo "[testdisk] 分区表:"
sfdisk -l "$OUT" | sed 's/^/    /'
echo "[testdisk] 数据分区内容:"
"$MDIR" -i "$PART1_IMG" ::/ | sed 's/^/    /'
echo "[testdisk] 用 QEMU（UEFI）启动: sh pack/run_qemu_ventoy.sh"
