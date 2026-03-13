// +build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
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

	// Make mount namespace private so our unmounts don't propagate to supervisord (kubectl exec needs /dev/pts).
	_ = unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
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

	// Unmount /sys and /dev so Android init gets a clean slate on restart.
	// First run creates /dev/pts, mounts sysfs, etc. On stop+start, those persist and init fails with "File exists" / "Device or resource busy".
	unmountForInitRestart()

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

// unmountForInitRestart clears /sys so Android init gets a clean slate on restart.
// Do NOT touch /dev - that would affect supervisord's namespace and break kubectl exec.
func unmountForInitRestart() {
	cpusetRoot := findCpusetRoot() // must read before unmounting /sys
	// 1. Unmount /dev/cpuset so we can unmount /sys (cpuset binds to /sys/fs/cgroup/cpuset).
	_ = unix.Unmount("/dev/cpuset", unix.MNT_DETACH)
	// 2. Unmount /sys.
	_ = unix.Unmount("/sys", unix.MNT_DETACH)
	// 3. Remount cpuset for init.rc (foreground/tasks). Can't bind from /sys (gone); mount cgroup cpuset directly.
	if cpusetRoot != "" {
		_ = os.MkdirAll("/dev/cpuset", 0755)
		if unix.Mount("cgroup", "/dev/cpuset", "cgroup", 0, "cpuset") == nil {
			_ = os.WriteFile("/dev/cpuset/cgroup.clone_children", []byte("1"), 0)
			_ = os.Symlink("cpuset.cpus", "/dev/cpuset/cpus")
			_ = os.Symlink("cpuset.mems", "/dev/cpuset/mems")
		}
	}
	// 4. Unmount/remove /dev/__properties__ so Android init can recreate it on restart.
	_ = unix.Unmount("/dev/__properties__", unix.MNT_DETACH)
	_ = os.RemoveAll("/dev/__properties__")

	// 5. Restore Calico default route. When init/netd dies, it may flush policy routing; Calico uses 169.254.1.1.
	restoreCalicoDefaultRoute()
}

func restoreCalicoDefaultRoute() {
	path := "ip"
	for _, p := range []string{"/shared/toybox-bin/ip", "/shared/busybox/bin/ip", "/sbin/ip", "/usr/sbin/ip"} {
		if _, err := os.Stat(p); err == nil {
			path = p
			break
		}
	}
	cmd := exec.Command(path, "route", "add", "default", "via", "169.254.1.1", "dev", "eth0")
	cmd.Env = append(os.Environ(), "PATH=/shared/bin:/shared/toybox-bin:/shared/busybox/bin:/sbin:/usr/sbin:/bin:/usr/bin")
	_ = cmd.Run() // ignore error (route may exist)
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
