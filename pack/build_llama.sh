#!/bin/sh
# 构建静态 llama-server（AI 环境内置的本地推理后端）
# 用法: build_llama.sh [v3|v2] [输出目录]
#   v3（默认）: x86-64-v3 / AVX2，现代 CPU 用，性能最好
#   v2:         x86-64-v2 / SSE4.2，老机器兜底（明显更慢）
# 产物: <输出目录>/llama-server.<变体>（静态链接、已 strip）
# 环境变量:
#   VTOY_AI_BUILD_DIR      输出目录（默认见 defaults.sh）
#   VTOY_AI_LLAMA_SRC      llama.cpp 源码目录（git 检出即可，只取 git 跟踪的文件）；
#                          不给则下载固定提交的源码归档并校验 SHA256
#   VTOY_AI_LLAMA_MIRROR   归档下载前缀（默认 GitHub 官方；国内可用 gh-proxy 镜像，
#                          例: https://gh-proxy.org/https://github.com/ggml-org/llama.cpp/archive）
#   VTOY_AI_LLAMA_SHA256   覆盖归档校验和（官方归档哈希未来若被 GitHub 重新生成时用）
#   VTOY_AI_BUILD_JOBS     并行度（默认 nproc）
set -e

. "$(dirname "$0")/defaults.sh"

VARIANT="${1:-v3}"
OUT="${2:-$VTOY_AI_BUILD_DIR}"

case "$VARIANT" in
    v3) MARCH=x86-64-v3; AVX=ON;  AVX2=ON;  FMA=ON;  F16C=ON;  BMI2=ON  ;;
    v2) MARCH=x86-64-v2; AVX=OFF; AVX2=OFF; FMA=OFF; F16C=OFF; BMI2=OFF ;;
    *)  echo "变体必须是 v3 或 v2（当前: $VARIANT）" >&2; exit 1 ;;
esac

# 固定提交与归档校验和：源码内容可复现，且换镜像/被篡改都会被检出
LLAMA_SHA="0c1e57098bba43ac29e6e3b677cdceebdd22334f"
ARCH_SHA="${VTOY_AI_LLAMA_SHA256:-6a85ec945b57789c875e1af90e66109fbf136a3c79802d5ca0333b043e10407f}"
MIRROR="${VTOY_AI_LLAMA_MIRROR:-https://github.com/ggml-org/llama.cpp/archive}"

command -v cmake >/dev/null || { echo "[llama] 需要 cmake（建议 >= 3.20）" >&2; exit 1; }
command -v tar >/dev/null   || { echo "[llama] 需要 tar" >&2; exit 1; }
mkdir -p "$OUT"

SRC="${VTOY_AI_LLAMA_SRC:-}"
WORK="$OUT/llama-cpp-src"
rm -rf "$WORK"
mkdir -p "$WORK"

if [ -n "$SRC" ]; then
    [ -d "$SRC" ] || { echo "源码目录不存在: $SRC" >&2; exit 1; }
    if [ -d "$SRC/.git" ] && command -v git >/dev/null; then
        # git archive 只导出跟踪文件：自动排除 build-*/ 等工作区构建产物
        echo "[llama] 从 git 检出导出源码: $SRC ($(git -C "$SRC" rev-parse --short HEAD 2>/dev/null || echo '?'))"
        git -C "$SRC" archive --format=tar HEAD | tar -xf - -C "$WORK"
    else
        echo "[llama] 复制源码目录: $SRC"
        tar -C "$SRC" --exclude=.git --exclude='./build*' -cf - . | tar -xf - -C "$WORK"
    fi
else
    command -v curl >/dev/null || { echo "[llama] 需要 curl（或显式给出 VTOY_AI_LLAMA_SRC）" >&2; exit 1; }
    TBZ="$OUT/dl/llama.cpp-$(echo "$LLAMA_SHA" | cut -c1-10).tar.gz"
    if [ ! -f "$TBZ" ] || ! echo "$ARCH_SHA  $TBZ" | sha256sum -c --quiet 2>/dev/null; then
        echo "[llama] 下载: $MIRROR/$LLAMA_SHA.tar.gz"
        mkdir -p "$OUT/dl"
        curl -fL --retry 3 -C - -o "$TBZ" "$MIRROR/$LLAMA_SHA.tar.gz"
        echo "$ARCH_SHA  $TBZ" | sha256sum -c || {
            echo "[llama] SHA256 校验失败。归档可能已被 GitHub 重新生成（用 VTOY_AI_LLAMA_SHA256 覆盖），"
            echo "        或改用 VTOY_AI_LLAMA_SRC 指定本地源码目录。" >&2
            exit 1
        }
    fi
    echo "[llama] 解压源码..."
    tar -xzf "$TBZ" -C "$WORK" --strip-components=1
fi
[ -f "$WORK/CMakeLists.txt" ] || { echo "源码目录不像 llama.cpp（缺 CMakeLists.txt）: $WORK" >&2; exit 1; }

echo "[llama] 配置 ($VARIANT, -march=$MARCH)..."
cmake -B "$WORK/build-$VARIANT" -S "$WORK" \
    -DCMAKE_BUILD_TYPE=Release \
    -DBUILD_SHARED_LIBS=OFF \
    -DGGML_NATIVE=OFF \
    -DGGML_OPENMP=OFF \
    -DGGML_CCACHE=OFF \
    -DLLAMA_CURL=OFF \
    -DLLAMA_OPENSSL=OFF \
    -DGGML_BUILD_EXAMPLES=OFF \
    -DGGML_BUILD_TESTS=OFF \
    -DGGML_AVX="$AVX" -DGGML_AVX2="$AVX2" -DGGML_FMA="$FMA" -DGGML_F16C="$F16C" \
    -DGGML_BMI2="$BMI2" -DGGML_SSE42=ON -DGGML_AVX_VNNI=OFF -DGGML_AVX512=OFF \
    -DCMAKE_C_FLAGS="-march=$MARCH" -DCMAKE_CXX_FLAGS="-march=$MARCH" \
    -DCMAKE_EXE_LINKER_FLAGS="-static" \
    >"$WORK/cmake.log" 2>&1 || { echo "[llama] cmake 配置失败，日志: $WORK/cmake.log" >&2; exit 1; }

echo "[llama] 编译 llama-server..."
cmake --build "$WORK/build-$VARIANT" --target llama-server \
    -j "${VTOY_AI_BUILD_JOBS:-$(nproc)}" >"$WORK/build.log" 2>&1 \
    || { echo "[llama] 编译失败，日志: $WORK/build.log" >&2; exit 1; }

BIN="$OUT/llama-server.$VARIANT"
cp "$WORK/build-$VARIANT/bin/llama-server" "$BIN"
strip --strip-all "$BIN" 2>/dev/null || true
chmod 755 "$BIN"

# 自检：宿主执行 --version（交叉为老 CPU 构建失败时仅提示，不算错误）
if "$BIN" --version >/dev/null 2>&1; then
    echo "[llama] 自检通过: $("$BIN" --version 2>&1 | head -1)"
else
    echo "[llama] 提示: 宿主自检未通过（若为更老/更新 ISA 交叉构建则正常）" >&2
fi

ls -la "$BIN"
sha256sum "$BIN"
echo "[llama] 完成: $BIN"
