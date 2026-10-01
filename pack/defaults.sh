# 构建路径的统一解析层：所有 pack/ 与 ime/ 脚本 source 本文件后再取默认值。
#
# 优先级: 位置参数 > 显式环境变量 > 机器本地 defaults.sh > 内置默认。
# 内置默认落在仓库外的用户缓存目录，构建产物一律不进仓库。
#
# 机器本地覆盖（本机私有路径写在这里，不要提交）:
#   ${VTOY_AI_DEFAULTS_FILE:-~/.config/ventoy-ai/defaults.sh}
#     export VTOY_AI_BUILD_DIR=/path/to/ventoy-ai-build
#     export VTOY_AI_BUSYBOX_SRC=/path/to/busybox-1.36.1   # 不给则 build_busybox.sh 自动下载
#     export VTOY_AI_LLAMA_SRC=/path/to/llama.cpp          # 不给则 build_llama.sh 自动下载归档
#     export VTOY_AI_VENTOY_RELEASE=/path/to/ventoy-1.1.05 # 仅 make_ventoy_testdisk.sh 需要
#
# 用法: . "$(dirname "$0")/defaults.sh"   （本文件可重复 source，只有首次生效）

if [ -n "${VTOY_AI_DEFAULTS_LOADED:-}" ]; then
    return 0
fi

# 已导出的环境变量要赢过本地文件里的赋值：先暂存，source 之后还原
ai_env_build_dir="${VTOY_AI_BUILD_DIR:-}"
ai_env_busybox_src="${VTOY_AI_BUSYBOX_SRC:-}"
ai_env_llama_src="${VTOY_AI_LLAMA_SRC:-}"
ai_env_ventoy_release="${VTOY_AI_VENTOY_RELEASE:-}"
ai_env_mtools_dir="${VTOY_AI_MTOOLS_DIR:-}"

ai_defaults_file="${VTOY_AI_DEFAULTS_FILE:-$HOME/.config/ventoy-ai/defaults.sh}"
if [ -f "$ai_defaults_file" ]; then
    . "$ai_defaults_file"
fi

if [ -n "$ai_env_build_dir" ]; then VTOY_AI_BUILD_DIR="$ai_env_build_dir"; fi
if [ -n "$ai_env_busybox_src" ]; then VTOY_AI_BUSYBOX_SRC="$ai_env_busybox_src"; fi
if [ -n "$ai_env_llama_src" ]; then VTOY_AI_LLAMA_SRC="$ai_env_llama_src"; fi
if [ -n "$ai_env_ventoy_release" ]; then VTOY_AI_VENTOY_RELEASE="$ai_env_ventoy_release"; fi
if [ -n "$ai_env_mtools_dir" ]; then VTOY_AI_MTOOLS_DIR="$ai_env_mtools_dir"; fi
unset ai_env_build_dir ai_env_busybox_src ai_env_llama_src ai_env_ventoy_release ai_env_mtools_dir ai_defaults_file

# 构建目录：内核/busybox/输入法/initramfs/测试盘的全部产物都在这里
: "${VTOY_AI_BUILD_DIR:="${XDG_CACHE_HOME:-$HOME/.cache}/ventoy-ai"}"
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
