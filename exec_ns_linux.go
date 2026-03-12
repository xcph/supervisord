// +build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

const execInNsMagic = "__exec_in_ns__"

// getSupervisordPath returns the path to the supervisord binary for re-exec.
// Avoids os.Executable() which reads /proc/self/exe and can fail in containers.
func getSupervisordPath() string {
	if p := os.Args[0]; p != "" {
		if filepath.IsAbs(p) {
			return p
		}
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	for _, p := range []string{"/shared/supervisord", "/usr/local/bin/supervisord"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("supervisord"); err == nil {
		return p
	}
	return "supervisord"
}

// runExecInNamespace spawns a child that joins the target process's PID and mount
// namespaces, then execs the given command. Uses pure Go (no external nsenter).
func runExecInNamespace(targetPid int, cmdArgs []string, stdin, stdout, stderr *os.File) error {
	self := getSupervisordPath()
	pidStr := strconv.Itoa(targetPid)
	args := append([]string{execInNsMagic, pidStr}, cmdArgs...)
	cmd := exec.Command(self, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// handleExecInNs runs when the process was invoked with __exec_in_ns__.
// It joins the target's namespaces and execs the command. Must be called
// before the normal main flow. Does not return on success (exec replaces process).
func handleExecInNs() bool {
	if len(os.Args) < 3 || os.Args[1] != execInNsMagic {
		return false
	}
	pidStr := os.Args[2]
	cmdArgs := os.Args[3:]
	if len(cmdArgs) == 0 {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/bash"
		}
		cmdArgs = []string{shell}
	}
	targetPid, err := strconv.Atoi(pidStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "exec_in_ns: invalid pid %q: %v\n", pidStr, err)
		os.Exit(1)
	}
	if err := execInNamespace(targetPid, cmdArgs); err != nil {
		fmt.Fprintf(os.Stderr, "exec_in_ns: %v\n", err)
		os.Exit(1)
	}
	return true
}

// execInNamespace joins the target process's PID and mount namespaces,
// then execs the command. Runs in the current process (child of runExecInNamespace).
//
// Go runtime is multithreaded; setns(CLONE_NEWNS) fails with EINVAL because the
// process shares CLONE_FS with other threads. We call unshare(CLONE_FS) first to
// give this thread its own copy of root/cwd, then setns can succeed.
//
// Order: unshare(CLONE_FS) -> setns(mnt) -> setns(pid). Mount before PID per
// setns(2) recommendations (user, mount, then other namespaces).
func execInNamespace(targetPid int, cmdArgs []string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	pidPath := filepath.Join("/proc", strconv.Itoa(targetPid), "ns", "pid")
	mntPath := filepath.Join("/proc", strconv.Itoa(targetPid), "ns", "mnt")

	pidFd, err := unix.Open(pidPath, unix.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open pid ns %s: %w", pidPath, err)
	}
	defer unix.Close(pidFd)

	mntFd, err := unix.Open(mntPath, unix.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open mnt ns %s: %w", mntPath, err)
	}
	defer unix.Close(mntFd)

	// Unshare CLONE_FS so we no longer share root/cwd with other threads.
	// Required for setns(mnt) to succeed in a multithreaded Go process.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("unshare CLONE_FS: %w", err)
	}

	// Join mount namespace first, then PID (per setns(2) ordering).
	if err := unix.Setns(mntFd, unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("setns mnt: %w", err)
	}
	if err := unix.Setns(pidFd, unix.CLONE_NEWPID); err != nil {
		return fmt.Errorf("setns pid: %w", err)
	}

	path := cmdArgs[0]
	argv := cmdArgs
	return unix.Exec(path, argv, os.Environ())
}
