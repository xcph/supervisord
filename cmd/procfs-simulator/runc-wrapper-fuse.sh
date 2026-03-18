#!/bin/sh
# runc 包装器（FUSE 版）：注入 hook-fuse.sh 作为 createContainer hook
# 独立脚本，与 procfs-simulator 配套使用
#
# 安装: cp runc-wrapper-fuse.sh /usr/local/bin/hide-overlayfs-runc
set -e

RUNC="${RUNC:-}"
for p in /usr/sbin/runc /usr/bin/runc; do
	[ -x "$p" ] && RUNC="$p" && break
done
[ -z "$RUNC" ] && RUNC="runc"
HOOK_SCRIPT="${HOOK_SCRIPT:-/usr/local/bin/hook-fuse.sh}"
BUNDLE=""
prev=""
for arg in "$@"; do
	[ "$prev" = "-b" ] && BUNDLE="$arg" && break
	prev="$arg"
done
[ -z "$BUNDLE" ] && BUNDLE="."
CONFIG="${BUNDLE}/config.json"

case "$1" in
	create)
		if [ -f "$CONFIG" ] && command -v jq >/dev/null 2>&1; then
			HOOK_JSON="{\"path\":\"$HOOK_SCRIPT\",\"args\":[\"hook-fuse\"]}"
			if jq -e '.hooks.createContainer' "$CONFIG" >/dev/null 2>&1; then
				jq --argjson hook "$HOOK_JSON" '.hooks.createContainer += [$hook]' "$CONFIG" > "${CONFIG}.tmp"
			else
				jq --argjson hook "$HOOK_JSON" '.hooks.createContainer = [$hook]' "$CONFIG" > "${CONFIG}.tmp"
			fi
			mv "${CONFIG}.tmp" "$CONFIG"
		fi
		;;
esac
exec "$RUNC" "$@"
