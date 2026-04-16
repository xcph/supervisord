//go:build linux
// +build linux

package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Default dirs under /shared masked inside container_run (tmpfs) so Android does not see host content.
// Supervisord reads etc/supervisord.conf in the parent before fork; the child namespace hides the file.
//
// Do not mask /shared/busybox or /shared/toybox-bin: program:init exec's WRAP_SHELL (e.g. /shared/busybox/bin/sh)
// in this mount namespace — tmpfs would remove the binary before exec.
var runContainerDefaultTmpfsMaskDirs = []string{
	"/shared/supervisord/etc",
	"/shared/bin",
}

// maskPathsForRunContainerAndroid hides host-mounted paths from the Android mount namespace.
//
// - /var/lib/cloudphone-node-agent: tmpfs overlay so agent.sock is not visible inside container_run.
//   Disable with CPH_RUN_CONTAINER_MASK_NODE_AGENT=0 or CPH_RUN_CONTAINER_KEEP_NODE_AGENT=1.
//
// - Default: /shared/supervisord/etc, /shared/bin (skip: CPH_RUN_CONTAINER_SKIP_DEFAULT_TMPFS_MASKS=1).
//   Add busybox/toybox via CPH_RUN_CONTAINER_TMPFS_MASK_DIRS only if init shell is not under those paths.
//
// - Extra comma-separated dirs: CPH_RUN_CONTAINER_TMPFS_MASK_DIRS=/path/a,/path/b
//
// We cannot mask the whole /shared/supervisord tree: this process still exec's from
// /shared/supervisord/supervisord and fork+exec's the same path for the helper child.
func maskPathsForRunContainerAndroid() {
	if os.Getenv("CPH_RUN_CONTAINER_MASK_NODE_AGENT") == "0" || os.Getenv("CPH_RUN_CONTAINER_KEEP_NODE_AGENT") == "1" {
		fmt.Fprintln(os.Stderr, "run_container: node-agent dir left visible (CPH_RUN_CONTAINER_MASK_NODE_AGENT=0 or CPH_RUN_CONTAINER_KEEP_NODE_AGENT=1)")
	} else {
		maskDirWithTmpfs("/var/lib/cloudphone-node-agent", "hide cloudphone-node-agent.sock from Android")
	}

	skipDef := os.Getenv("CPH_RUN_CONTAINER_SKIP_DEFAULT_TMPFS_MASKS")
	if skipDef != "1" && !strings.EqualFold(skipDef, "true") && !strings.EqualFold(skipDef, "yes") {
		for _, p := range runContainerDefaultTmpfsMaskDirs {
			maskDirWithTmpfs(p, "default /shared hide from Android")
		}
	}

	extra := strings.TrimSpace(os.Getenv("CPH_RUN_CONTAINER_TMPFS_MASK_DIRS"))
	if extra == "" {
		return
	}
	for _, p := range strings.Split(extra, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		maskDirWithTmpfs(p, "CPH_RUN_CONTAINER_TMPFS_MASK_DIRS")
	}
}

func maskDirWithTmpfs(path, reason string) {
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		return
	}
	if err := unix.Mount("tmpfs", path, "tmpfs", 0, "size=4m,mode=0755"); err != nil {
		fmt.Fprintf(os.Stderr, "run_container: tmpfs mask %q (%s): %v\n", path, reason, err)
		return
	}
	fmt.Fprintf(os.Stderr, "run_container: tmpfs masked %q (%s)\n", path, reason)
}
