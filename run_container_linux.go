// +build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const runContainerMagic = "__run_container__"

// handleRunContainer runs when the process was invoked as:
//   supervisord __run_container__ <target> [args...]
// The process is the child forked with container namespaces (PID, mount, cgroup, UTS, IPC).
// Network ns is excluded by default so redroid netd can use iptables (pod network).
func handleRunContainer() bool {
	if len(os.Args) < 3 || os.Args[1] != runContainerMagic {
		return false
	}
	target := os.Args[2]
	argv := os.Args[2:]

	// Mount fresh /proc for the new PID namespace (we are PID 1 here).
	_ = unix.Unmount("/proc", unix.MNT_DETACH)
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		fmt.Fprintf(os.Stderr, "run_container: mount /proc: %v\n", err)
		os.Exit(1)
	}

	// If we have a new network namespace (container_network_isolated=true), bring up lo.
	// When using pod network (default), lo is already up.
	setupLoopback()

	// Setup cpuset so redroid init can write to /dev/cpuset/foreground/tasks.
	setupCpusetForRedroid()
	if err := unix.Exec(target, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "run_container: exec %s: %v\n", target, err)
		os.Exit(1)
	}
	return true
}

func setupLoopback() {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	_ = unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

func setupCpusetForRedroid() {
	cpusetRoot := findCpusetRoot()
	if cpusetRoot == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(cpusetRoot, "cgroup.clone_children")); err == nil {
		_ = os.WriteFile(filepath.Join(cpusetRoot, "cgroup.clone_children"), []byte("1"), 0)
	}
	for _, f := range []string{"cpuset.cpus", "cpuset.mems"} {
		p := filepath.Join(cpusetRoot, f)
		data, _ := os.ReadFile(p)
		if len(data) == 0 || len(strings.TrimSpace(string(data))) == 0 {
			_ = os.WriteFile(p, []byte("0"), 0)
		}
	}
	_ = os.MkdirAll("/dev/cpuset", 0755)
	_ = unix.Mount(cpusetRoot, "/dev/cpuset", "", unix.MS_BIND, "")
	// Android init.rc expects /dev/cpuset/cpus and /dev/cpuset/mems (copy command).
	// Cgroup v1 provides cpuset.cpus and cpuset.mems. Add symlinks for compatibility.
	_ = os.Symlink("cpuset.cpus", "/dev/cpuset/cpus")
	_ = os.Symlink("cpuset.mems", "/dev/cpuset/mems")
}

func findCpusetRoot() string {
	for _, root := range []string{"/sys/fs/cgroup/cpuset", "/sys/fs/cgroup"} {
		if _, err := os.Stat(filepath.Join(root, "cpuset.cpus")); err == nil {
			return root
		}
		if _, err := os.Stat(filepath.Join(root, "cpuset.cpus.effective")); err == nil {
			return root
		}
	}
	return ""
}
