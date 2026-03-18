#!/bin/sh
# OCI createContainer hook: 用 FUSE 虚拟 /proc 替换真实 /proc，
# 在 mounts/mountinfo 的 read 中返回过滤后的内容（overlay→ext4）。
#
# 需配合 runc-wrapper 使用，且宿主机需安装 procfs-simulator 二进制。

set -e

FUSE_BIN="${FUSE_BIN:-/usr/local/bin/procfs-simulator}"
PROC_REAL="${PROC_REAL:-/proc.real}"

# 检查是否已有 overlay（无则跳过）
if ! grep -qE 'overlay|overlayfs' /proc/mounts 2>/dev/null; then
	exit 0
fi

# 1. 将真实 /proc 绑定到 /proc.real
if [ ! -d "$PROC_REAL" ]; then
	mkdir -p "$PROC_REAL" 2>/dev/null || true
fi
mount --bind /proc "$PROC_REAL" 2>/dev/null || exit 0

# 2. 卸载当前 /proc（lazy umount，避免占用）
umount -l /proc 2>/dev/null || exit 0

# 3. 后台启动 FUSE，挂载到 /proc
if [ -x "$FUSE_BIN" ]; then
	"$FUSE_BIN" -real="$PROC_REAL" -mount=/proc &
	disown
fi

exit 0
