#!/bin/bash
# 在 Docker 中构建 procfs-simulator，支持 amd64/arm64（纯 Go，CGO_ENABLED=0）
# 用法: ./build.sh [amd64|arm64]
set -e

arch="${1:-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')}"
case "$arch" in
    amd64|x86_64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "Usage: $0 [amd64|arm64]"; exit 1 ;;
esac

echo "Building for $arch..."
docker build -f Dockerfile.build \
    --build-arg TARGETARCH=$arch \
    --build-arg TARGETOS=linux \
    -t procfs-simulator-builder .

docker run --rm -v "$(pwd):/out" procfs-simulator-builder sh -c "cp /src/procfs-simulator /src/hook-fuse.sh /out/"
chmod +x procfs-simulator hook-fuse.sh
echo "Built: ./procfs-simulator, ./hook-fuse.sh"
