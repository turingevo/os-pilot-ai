#!/bin/sh
set -e
cd "$(dirname "$0")"

# 自绘屏幕点阵字体不再嵌入本二进制：由 pack/pack_env.sh 生成/打包为
# initramfs 内的独立数据文件 /ventoy/ai/screen/font.bin（agent 运行时从文件加载）。

echo "构建 os-pilot-ai（静态链接，linux/amd64）..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o os-pilot-ai .

echo "完成: $(pwd)/os-pilot-ai"
ls -la os-pilot-ai
