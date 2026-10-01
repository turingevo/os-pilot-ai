#!/bin/sh
# 一键把本地模型所需文件下载到 Ventoy U 盘（在电脑上运行，不是 U 盘里）
#
# 用法: fetch_local_llm.sh <U盘挂载点> [模型文件名]
#   下载 llama-server 到 <挂载点>/ventoy/ai/llama-server
#   下载模型到       <挂载点>/ventoy/ai/models/<模型文件名>
#   放好后无需任何配置：从 U 盘启动即默认使用本地模型。
#
# 环境变量（可选）:
#   VTOY_AI_LLM_BIN_URL       llama-server 下载地址（默认 GitHub Release 的 v3/AVX2 版）
#   VTOY_AI_LLM_BIN_SHA256    二进制 SHA256（默认内置 v3 的；置空则跳过校验）
#   VTOY_AI_LLM_ENDPOINT      HuggingFace 镜像前缀（默认 https://huggingface.co，
#                             国内可设 https://hf-mirror.com）
#   VTOY_AI_LLM_MODEL_URL     模型完整下载地址（默认 Qwen/Qwen3.5-4B-GGUF 官方仓库）
#   VTOY_AI_LLM_MODEL_SHA256  模型 SHA256（默认内置默认模型的；换模型时必须提供）
#
# 示例:
#   fetch_local_llm.sh /media/$USER/Ventoy
#   fetch_local_llm.sh /mnt/usb Qwen3.5-4B-UD-Q4_K_XL.gguf
#   VTOY_AI_LLM_BIN_URL=https://gh-proxy.org/https://github.com/... fetch_local_llm.sh /mnt/usb
set -e

BIN_URL="${VTOY_AI_LLM_BIN_URL:-https://github.com/turingevo/os-pilot-ai/releases/latest/download/llama-server.v3}"
BIN_SHA="${VTOY_AI_LLM_BIN_SHA256-a8579cc8a386b8b3cc1ba7d657435f43cce27ea08c64db9dae4ae216346a2664}"
ENDPOINT="${VTOY_AI_LLM_ENDPOINT:-https://huggingface.co}"
MODEL_REPO="Qwen/Qwen3.5-4B-GGUF"
MODEL_FILE_DEFAULT="Qwen3.5-4B-Q4_K_M.gguf"
MODEL_SHA_DEFAULT="00fe7986ff5f6b463e62455821146049db6f9313603938a70800d1fb69ef11a4"
MODEL_BYTES_DEFAULT=2740937888

MNT="${1:-}"
MODEL_FILE="${2:-$MODEL_FILE_DEFAULT}"
[ -n "$MNT" ] || { echo "用法: fetch_local_llm.sh <U盘挂载点> [模型文件名]" >&2; exit 1; }
[ -d "$MNT" ] || { echo "挂载点不存在: $MNT" >&2; exit 1; }
[ -w "$MNT" ] || { echo "挂载点不可写: $MNT" >&2; exit 1; }
command -v curl >/dev/null || { echo "需要 curl" >&2; exit 1; }

MODEL_URL="${VTOY_AI_LLM_MODEL_URL:-$ENDPOINT/$MODEL_REPO/resolve/main/$MODEL_FILE}"
MODEL_SHA="${VTOY_AI_LLM_MODEL_SHA256:-}"
if [ -z "$MODEL_SHA" ] && [ "$MODEL_FILE" = "$MODEL_FILE_DEFAULT" ]; then
    MODEL_SHA="$MODEL_SHA_DEFAULT"
fi

AI_DIR="$MNT/ventoy/ai"
BIN_DST="$AI_DIR/llama-server"
MODEL_DST="$AI_DIR/models/$MODEL_FILE"
mkdir -p "$AI_DIR/models"

# ---------- llama-server ----------
if [ -f "$BIN_DST" ] && [ -n "$BIN_SHA" ] && \
   echo "$BIN_SHA  $BIN_DST" | sha256sum -c --quiet 2>/dev/null; then
    echo "[fetch] llama-server 已存在且校验通过，跳过下载"
else
    if [ -f "$BIN_DST" ] && [ -n "$BIN_SHA" ]; then
        echo "[fetch] 错误: $BIN_DST 已存在但校验不符（若是你自备的二进制请移走再运行）" >&2
        exit 1
    fi
    echo "[fetch] 下载 llama-server: $BIN_URL"
    curl -fL --retry 3 -C - -o "$BIN_DST" "$BIN_URL"
    if [ -n "$BIN_SHA" ]; then
        echo "$BIN_SHA  $BIN_DST" | sha256sum -c || {
            echo "[fetch] 校验失败: $BIN_DST" >&2
            exit 1
        }
    else
        echo "[fetch] 警告: 未提供 SHA256，跳过二进制校验" >&2
    fi
    chmod 755 "$BIN_DST"
fi

# ---------- GGUF 模型 ----------
# 目标磁盘空间预检：优先用服务器 Content-Length，拿不到时用默认模型体积估算
MODEL_BYTES=$(curl -fsSIL --connect-timeout 10 "$MODEL_URL" 2>/dev/null | tr -d '\r' | \
    awk 'tolower($1) == "content-length:" { v = $2 } END { print v }')
case "$MODEL_BYTES" in
    ''|*[!0-9]*) MODEL_BYTES="" ;;
esac
if [ -z "$MODEL_BYTES" ] && [ "$MODEL_FILE" = "$MODEL_FILE_DEFAULT" ]; then
    MODEL_BYTES=$MODEL_BYTES_DEFAULT
fi
if [ -n "$MODEL_BYTES" ]; then
    NEED_KB=$(( (MODEL_BYTES + 134217728) / 1024 ))  # 模型体积 + 128MiB 余量
    AVAIL_KB=$(df -kP "$MNT" | awk 'NR==2 {print $4}')
    if [ -n "$AVAIL_KB" ] && [ "$AVAIL_KB" -lt "$NEED_KB" ] 2>/dev/null; then
        echo "[fetch] 错误: 空间不足（可用 $((AVAIL_KB / 1024 / 1024))GiB，模型约需 $((MODEL_BYTES / 1024 / 1024 / 1024))GiB）" >&2
        exit 1
    fi
fi

if [ -f "$MODEL_DST" ] && [ -n "$MODEL_SHA" ] && \
   echo "$MODEL_SHA  $MODEL_DST" | sha256sum -c --quiet 2>/dev/null; then
    echo "[fetch] 模型已存在且校验通过，跳过下载"
else
    if [ -f "$MODEL_DST" ] && [ -n "$MODEL_SHA" ]; then
        echo "[fetch] 错误: $MODEL_DST 已存在但校验不符（请移走或删除后重试）" >&2
        exit 1
    fi
    echo "[fetch] 下载模型: $MODEL_URL"
    curl -fL --retry 3 -C - -o "$MODEL_DST" "$MODEL_URL"
    if [ -n "$MODEL_SHA" ]; then
        echo "[fetch] 校验模型（约 2.5GiB，稍等）..."
        echo "$MODEL_SHA  $MODEL_DST" | sha256sum -c || {
            echo "[fetch] 校验失败: $MODEL_DST" >&2
            exit 1
        }
    else
        echo "[fetch] 警告: 非默认模型且未提供 VTOY_AI_LLM_MODEL_SHA256，跳过校验" >&2
    fi
fi

echo ""
echo "[fetch] 完成:"
ls -la "$BIN_DST" "$MODEL_DST"
echo ""
echo "使用方法: 安全弹出 U 盘后从 U 盘启动；无需 ai.json，AI 装机助手默认使用本地模型。"
echo "提示: 本地 CPU 推理首次回答较慢（分钟级）；CPU 不支持 AVX2 时请换用 Release 中的 llama-server.v2。"
