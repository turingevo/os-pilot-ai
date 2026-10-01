# 构建路径的兜底默认层：所有 pack/ 与 ime/ 脚本 source 本文件后再取默认值。
#
# 优先级只有两条来源：位置参数（各脚本自己读 $1）> 环境变量 > 本文件的内置默认。
# 开发机的本机配置不在这里，而在仓库根的 dev.env.sh（`source` 它即导出环境变量，
# 模板见 dev.env.sh.example）。本文件不再读任何用户配置文件，所以 source 一次环境后
# 所有脚本直接可跑，单次覆盖也只是命令行的前缀。
#
# 构建产物一律落在仓库外：没配置时内置默认用 ${XDG_CACHE_HOME:-$HOME/.cache}/ventoy-ai，
# 并打一行提示（见下）。
#
# 用法: . "$(dirname "$0")/defaults.sh"   （本文件可重复 source，只有首次生效）

if [ -n "${VTOY_AI_DEFAULTS_LOADED:-}" ]; then
    return 0
fi

# 构建目录：内核/busybox/输入法/initramfs/测试盘的全部产物都在这里
if [ -z "${VTOY_AI_BUILD_DIR:-}" ]; then
    : "${VTOY_AI_BUILD_DIR:="${XDG_CACHE_HOME:-$HOME/.cache}/ventoy-ai"}"
    echo "[defaults] 提示: 未设置 VTOY_AI_BUILD_DIR，本次用内置默认 $VTOY_AI_BUILD_DIR" >&2
    echo "[defaults]        开发机请先在仓库根 source 本机配置：. ./dev.env.sh（模板 dev.env.sh.example）" >&2
fi
export VTOY_AI_BUILD_DIR

# QEMU 加速与资源：所有 run_qemu*/verify_* 脚本共用，VTOY_AI_QEMU_* 可覆盖。
# ACCEL: 能读写 /dev/kvm 就走 KVM —— x86 上 TCG 纯模拟比 KVM 慢一个数量级，且默认
#   CPU 模型 qemu64 缺 AVX 等指令；要强制软件模拟给 VTOY_AI_QEMU_ACCEL='-accel tcg'。
# MEM/SMP: 4G/4 核。桌面版 ISO（>4G）经 Ventoy 引导时，1G 内存会因解包 initrd 写满
#   内存根（ramfs）而 /init not found → kernel panic，所以不要用 1G 这类小内存。
if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
    : "${VTOY_AI_QEMU_ACCEL:=-accel kvm -cpu host}"
else
    : "${VTOY_AI_QEMU_ACCEL:=}"
    echo "[defaults] 提示: /dev/kvm 不可读写，QEMU 退回 TCG 纯模拟（明显更慢）" >&2
fi
: "${VTOY_AI_QEMU_MEM:=4096}"
: "${VTOY_AI_QEMU_SMP:=4}"
export VTOY_AI_QEMU_ACCEL VTOY_AI_QEMU_MEM VTOY_AI_QEMU_SMP

VTOY_AI_DEFAULTS_LOADED=1
export VTOY_AI_DEFAULTS_LOADED
