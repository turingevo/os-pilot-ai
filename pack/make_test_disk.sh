#!/bin/sh
# 生成 QEMU 测试用 payload 磁盘（ext4，内含假 ISO/应答脚本/ventoy.json/AI 环境文件）
# 用法: make_test_disk.sh [输出镜像] [内核 vmlinuz] [initrd]
# 环境变量:
#   VTOY_AI_BUILD_DIR       构建目录（默认见 defaults.sh；镜像/内核/initrd 都从这里取）
#   VTOY_AI_KEEP_PAYLOAD=1  不重建 payload 目录，仅把同名目录重新打包成镜像
#                           （保留手工改过的 ventoy/ai.json；下面几个变量此时不生效）
#   VTOY_AI_BASE_URL        大模型接口地址，QEMU 内 10.0.2.2 即宿主机
#   VTOY_AI_MODEL           模型名
#   VTOY_AI_REQUEST_TIMEOUT 请求超时（秒）
#   VTOY_AI_WITH_SCRIPT=0   不生成 test_script.txt（交互式镜像，而非自动脚本模式）
#   VTOY_AI_SCRIPT_FILE     用外部脚本文件替代内置的 test_script.txt（需 WITH_SCRIPT=1）
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
IMG="${1:-$BUILD/test/payload.img}"
KERNEL="${2:-$BUILD/kernel/vmlinuz}"
INITRD="${3:-$BUILD/initrd-ai.cpio.gz}"
AI_DIR="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(dirname "$IMG")"
mkdir -p "$TEST_DIR"
TEST_DIR="$(cd "$TEST_DIR" && pwd)"
PAYLOAD="$TEST_DIR/$(basename "$IMG" .img)"
KEEP_PAYLOAD="${VTOY_AI_KEEP_PAYLOAD:-0}"

# AI 配置（可用环境变量覆盖）
BASE_URL="${VTOY_AI_BASE_URL:-http://10.0.2.2:18800/v1}"
MODEL="${VTOY_AI_MODEL:-mock-model}"
REQ_TIMEOUT="${VTOY_AI_REQUEST_TIMEOUT:-30}"
WITH_SCRIPT="${VTOY_AI_WITH_SCRIPT:-1}"
SCRIPT_FILE="${VTOY_AI_SCRIPT_FILE:-}"

if [ "$KEEP_PAYLOAD" = "1" ]; then
    [ -d "$PAYLOAD" ] || { echo "payload 目录不存在: $PAYLOAD" >&2; exit 1; }
    rm -f "$IMG"
else
    rm -rf "$PAYLOAD" "$IMG"
    mkdir -p "$PAYLOAD/ventoy/ai" "$PAYLOAD/scripts"

    # 假 ISO（大于 Ventoy 的 32KB 过滤阈值）
    dd if=/dev/urandom of="$PAYLOAD/ubuntu-24.04-desktop-amd64.iso" bs=1024 count=64 2>/dev/null
    dd if=/dev/urandom of="$PAYLOAD/debian-12-netinst-amd64.iso" bs=1024 count=40 2>/dev/null

    # 应答脚本
    cat > "$PAYLOAD/scripts/ubuntu.seed" <<'EOF'
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

    # 已有 ventoy.json：验证 schedule_boot 能保留其它字段并正确合并
    cat > "$PAYLOAD/ventoy/ventoy.json" <<'EOF'
{
    "theme": {
        "file": "/ventoy/theme/theme.txt"
    },
    "control": {
        "VTOY_MENU_TIMEOUT": 30
    }
}
EOF

    # 端到端测试脚本（agent 以 --script 方式读取）
    # 行首为 ! 的行会被 agent 当本地命令直执（复用 run_command 安全层），不经模型；
    # 脚本模式下确认自动通过，因此可用纯命令脚本做「无 LLM」的工具链实测。
    if [ "$WITH_SCRIPT" = "1" ]; then
        if [ -n "$SCRIPT_FILE" ]; then
            [ -f "$SCRIPT_FILE" ] || { echo "外部脚本不存在: $SCRIPT_FILE" >&2; exit 1; }
            cp "$SCRIPT_FILE" "$PAYLOAD/ventoy/ai/test_script.txt"
        else
            cat > "$PAYLOAD/ventoy/ai/test_script.txt" <<'EOF'
你好，请先看看U盘里有哪些镜像
帮我给 ubuntu-24.04-desktop-amd64.iso 配置自动安装，用 /scripts/ubuntu.seed
确认写入配置
再帮我检查一下磁盘环境
EOF
        fi
    fi

    # AI 配置：QEMU 内指向宿主机服务（10.0.2.2 = slirp 网关）
    cat > "$PAYLOAD/ventoy/ai.json" <<EOF
{
    "provider": "openai-compatible",
    "base_url": "$BASE_URL",
    "model": "$MODEL",
    "api_key": "",
    "temperature": 0.2,
    "max_tokens": 262144,
    "language": "zh-CN",
    "mode": "orchestrate",
    "request_timeout": $REQ_TIMEOUT,
    "payload_dir": "/iso",
    "log_dir": "/iso/ventoy/ai/logs"
}
EOF
fi

# AI 环境文件（对应 T3 入口的 /ventoy/ai/ 布局；两种模式都刷新）
mkdir -p "$PAYLOAD/ventoy/ai"
if [ -f "$KERNEL" ]; then
    cp "$KERNEL" "$PAYLOAD/ventoy/ai/vmlinuz"
else
    echo "警告: 内核不存在 $KERNEL，跳过 vmlinuz" >&2
fi
if [ -f "$INITRD" ]; then
    cp "$INITRD" "$PAYLOAD/ventoy/ai/initrd"
else
    echo "警告: initrd 不存在 $INITRD，跳过 initrd" >&2
fi

# 生成 ext4 镜像（mke2fs -d 无需 root 挂载）
dd if=/dev/zero of="$IMG" bs=1M count=80 2>/dev/null
mke2fs -q -t ext4 -F -d "$PAYLOAD" "$IMG" 81920 2>/dev/null || {
    # 兼容旧版 mke2fs（不支持 -d）：先建空文件系统，再用 debugfs 写入
    echo "mke2fs -d 不可用，改用 debugfs 写入" >&2
    mke2fs -q -t ext4 -F "$IMG" 81920
    (cd "$PAYLOAD" && find . -type f | while read -r f; do
        debugfs -w -R "write $f ${f#./}" "$IMG" >/dev/null 2>&1
    done)
}

echo "[make_test_disk] 完成: $IMG"
if [ "$KEEP_PAYLOAD" = "1" ]; then
    echo "[make_test_disk] keep_payload=1（沿用现有 $PAYLOAD）"
else
    echo "[make_test_disk] base_url=$BASE_URL model=$MODEL timeout=$REQ_TIMEOUT script=$WITH_SCRIPT"
fi
ls -la "$IMG"
