# 构建 procfs-simulator 及 FUSE hook（纯 Go，CGO_ENABLED=0）
FROM golang:1.21-bookworm AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download 2>/dev/null || true
COPY *.go ./
RUN CGO_ENABLED=0 go build -o procfs-simulator .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends fuse3 libfuse3-3 && rm -rf /var/lib/apt/lists/*
COPY --from=builder /src/procfs-simulator /usr/local/bin/
COPY hook-fuse.sh /usr/local/bin/hook-fuse.sh
RUN chmod +x /usr/local/bin/procfs-simulator /usr/local/bin/hook-fuse.sh
# 用于复制到节点的入口
CMD ["cat", "/usr/local/bin/procfs-simulator"]
