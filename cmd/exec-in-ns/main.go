//go:build linux
// +build linux

// exec-in-ns: minimal binary to join target's namespaces and exec a command.
// Standalone to avoid supervisord's dependency chain that triggers /proc/self/exe (fails in containers).
// Joins: user, mnt, pid, net, uts, ipc, cgroup namespaces.
// When stdin is a TTY, wraps in a PTY to fix "can't set tty process group" in init's PID namespace.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Linux namespace constants (from syscall package, not always in unix)
const (
	cloneNewUser   = 0x10000000
	cloneNewNS     = 0x00020000
	cloneNewPID    = 0x20000000
	cloneNewNet    = 0x40000000
	cloneNewUTS    = 0x04000000
	cloneNewIPC    = 0x08000000
	cloneNewCgroup = 0x02000000
	cloneFS        = 0x00000200
)

func main() {
	args := os.Args[1:]
	inner := false
	usePty := false
	for len(args) > 0 && (args[0] == "--inner" || args[0] == "--pty") {
		if args[0] == "--inner" {
			inner = true
		} else {
			usePty = true
		}
		args = args[1:]
	}
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: exec-in-ns [--inner] [--pty] <target_pid> [cmd] [args...]\n")
		os.Exit(1)
	}
	targetPid, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "exec-in-ns: invalid pid %q: %v\n", args[0], err)
		os.Exit(1)
	}
	cmdArgs := args[1:]
	if len(cmdArgs) == 0 {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmdArgs = []string{shell}
	}

	// Use PTY when: (1) stdin is TTY, or (2) running interactive shell (no args or shell as cmd).
	// Ctl may run via RPC so server's stdin is not a TTY; always use PTY for shell to fix "can't set tty process group".
	// IMPORTANT: PTY must be created AFTER joining target's PID namespace, else tcsetpgrp fails with "No such process".
	// grpctunnel/cph-agentctl without -t sets EXEC_IN_NS_NO_PTY=1: otherwise shell opens on PTY and blocks
	// waiting for input while the client has no stdin attached.
	noPty := os.Getenv("EXEC_IN_NS_NO_PTY") == "1"
	needPty := !inner && !noPty && (isTTY(int(os.Stdin.Fd())) || isInteractiveShell(cmdArgs))
	if needPty {
		if err := runWithPTY(targetPid, cmdArgs); err != nil {
			fmt.Fprintf(os.Stderr, "exec-in-ns: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if inner && usePty {
		if err := execInNamespaceWithPTY(targetPid, cmdArgs); err != nil {
			fmt.Fprintf(os.Stderr, "exec-in-ns: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := execInNamespace(targetPid, cmdArgs); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "exec-in-ns: %v\n", err)
		os.Exit(1)
	}
}

func isTTY(fd int) bool {
	return term.IsTerminal(fd)
}

func isInteractiveShell(cmdArgs []string) bool {
	if len(cmdArgs) == 0 {
		return true
	}
	cmd := cmdArgs[0]
	return cmd == "/bin/sh" || cmd == "/bin/bash" || cmd == "sh" || cmd == "bash" ||
		cmd == "/shared/busybox/bin/sh" || cmd == "/shared/toybox-bin/sh" || cmd == "/system/bin/sh"
}

func runWithPTY(targetPid int, cmdArgs []string) error {
	self := getSelfPath()
	// PTY is created inside target's PID namespace (--inner --pty) to fix "can't set tty process group".
	innerArgs := append([]string{"--inner", "--pty", strconv.Itoa(targetPid)}, cmdArgs...)
	cmd := exec.Command(self, innerArgs...)
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			// Inner process has its own PTY; SIGWINCH will be forwarded when we add pty.InheritSize there.
		}
	}()
	ch <- syscall.SIGWINCH
	defer func() { signal.Stop(ch); close(ch) }()

	if term.IsTerminal(int(os.Stdin.Fd())) {
		oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return err
		}
		defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
	}
	return cmd.Run()
}

func joinNamespaces(targetPid int) error {
	procBase := filepath.Join("/proc", strconv.Itoa(targetPid), "ns")
	nsNames := []string{"user", "mnt", "pid", "net", "uts", "ipc", "cgroup"}
	nsTypes := []int{cloneNewUser, cloneNewNS, cloneNewPID, cloneNewNet, cloneNewUTS, cloneNewIPC, cloneNewCgroup}
	fds := make([]int, len(nsNames))
	defer func() {
		for _, fd := range fds {
			if fd > 0 {
				unix.Close(fd)
			}
		}
	}()

	for i, name := range nsNames {
		path := filepath.Join(procBase, name)
		fd, err := unix.Open(path, unix.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		fds[i] = fd
	}

	if err := unix.Unshare(cloneFS); err != nil {
		return fmt.Errorf("unshare CLONE_FS: %w", err)
	}

	skipOnErr := map[string]bool{"user": true, "cgroup": true}
	debug := os.Getenv("EXEC_IN_NS_DEBUG") != ""
	for i, fd := range fds {
		if err := unix.Setns(fd, nsTypes[i]); err != nil {
			if skipOnErr[nsNames[i]] && (errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EPERM)) {
				// Common in unprivileged containers; avoid stderr noise unless EXEC_IN_NS_DEBUG=1.
				if debug {
					fmt.Fprintf(os.Stderr, "exec-in-ns: skip %s ns (setns: %v)\n", nsNames[i], err)
				}
				continue
			}
			return fmt.Errorf("setns %s: %w", nsNames[i], err)
		}
	}
	return nil
}

// execInNamespaceWithPTY joins target's namespaces, creates PTY *inside* the target PID namespace,
// then runs the shell with that PTY. This fixes "can't set tty process group: No such process"
// because the PTY's process group is valid in the target namespace.
func execInNamespaceWithPTY(targetPid int, cmdArgs []string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := joinNamespaces(targetPid); err != nil {
		return err
	}

	ptmx, pts, err := pty.Open()
	if err != nil {
		return fmt.Errorf("pty.Open: %w", err)
	}
	defer ptmx.Close()
	defer pts.Close()

	if term.IsTerminal(int(os.Stdin.Fd())) {
		_ = pty.InheritSize(os.Stdin, ptmx)
	} else {
		// gRPC / pipe stdin is not a TTY: winsize must come from EXEC_IN_NS_ROWS/COLS (see cloudphone ExecHandshake).
		applyEnvWinsizeToPTMX(ptmx)
	}

	path, argv := resolveShell(cmdArgs)
	cmd := exec.Command(path, argv[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdin = pts
	cmd.Stdout = pts
	cmd.Stderr = pts
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0, // stdin (pts) is index 0 in child's ProcAttr.Files
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start shell: %w", err)
	}
	pts.Close() // child has it

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			if term.IsTerminal(int(os.Stdin.Fd())) {
				_ = pty.InheritSize(os.Stdin, ptmx)
			} else {
				applyEnvWinsizeToPTMX(ptmx)
			}
		}
	}()
	if term.IsTerminal(int(os.Stdin.Fd())) {
		ch <- syscall.SIGWINCH
	}
	defer func() { signal.Stop(ch); close(ch) }()

	if term.IsTerminal(int(os.Stdin.Fd())) {
		oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return err
		}
		defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
	}

	go func() { _, _ = io.Copy(ptmx, os.Stdin) }()
	_, _ = io.Copy(os.Stdout, ptmx)
	return cmd.Wait()
}

func isShellCommandName(path string) bool {
	switch filepath.Base(path) {
	case "sh", "bash", "ash", "ksh":
		return true
	default:
		return false
	}
}

// resolveShell maps a bare shell name to a known busybox/system path. It must not run for arbitrary
// commands: e.g. "ps" with no cwd hit would wrongly become sh + "-ef" and produce no useful output.
func resolveShell(cmdArgs []string) (path string, argv []string) {
	path = cmdArgs[0]
	argv = append([]string(nil), cmdArgs...)
	if !isShellCommandName(path) {
		return path, argv
	}
	shellFallbacks := []string{"/shared/busybox/bin/sh", "/shared/toybox-bin/sh", "/system/bin/sh", "/bin/sh"}
	if !pathExists(path) || path == "/system/bin/sh" || path == "/bin/sh" {
		for _, p := range shellFallbacks {
			if pathExists(p) {
				path = p
				argv = append([]string{p}, cmdArgs[1:]...)
				break
			}
		}
	}
	return path, argv
}

func execInNamespace(targetPid int, cmdArgs []string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := joinNamespaces(targetPid); err != nil {
		return err
	}

	path, argv := resolveShell(cmdArgs)
	if !filepath.IsAbs(path) {
		if lp, err := exec.LookPath(path); err == nil {
			path = lp
			if len(argv) > 0 {
				argv[0] = lp
			}
		}
	}
	// 使用 fork+exec，不用 unix.Exec 直接替换当前进程：在 setns 进入目标 PID 命名空间后，
	// 部分 Android/toybox 对「由宿主进程 setns 再 execve 得到」的进程上 stat("/proc/self/exe") 会失败；
	// 带 -t 时走 execInNamespaceWithPTY（子进程 exec）则正常。子进程在命名空间内原生 fork 可避免该问题。
	cmd := exec.Command(path, argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// applyEnvWinsizeToPTMX sets TIOCSWINSZ when stdin is not a TTY (e.g. grpctunnel pipe).
func applyEnvWinsizeToPTMX(f *os.File) {
	rows, cols, ok := winsizeFromEnv()
	if !ok {
		rows, cols = 24, 80
	}
	_ = pty.Setsize(f, &pty.Winsize{Rows: rows, Cols: cols})
}

func winsizeFromEnv() (rows, cols uint16, ok bool) {
	rs := os.Getenv("EXEC_IN_NS_ROWS")
	cs := os.Getenv("EXEC_IN_NS_COLS")
	if rs == "" || cs == "" {
		return 0, 0, false
	}
	r, err1 := strconv.Atoi(rs)
	c, err2 := strconv.Atoi(cs)
	if err1 != nil || err2 != nil || r <= 0 || c <= 0 || r > 65535 || c > 65535 {
		return 0, 0, false
	}
	return uint16(r), uint16(c), true
}

func getSelfPath() string {
	if p := os.Getenv("EXEC_IN_NS_PATH"); p != "" && pathExists(p) {
		return p
	}
	if arg0 := os.Args[0]; arg0 != "" && arg0[0] == '/' && pathExists(arg0) {
		return arg0
	}
	for _, p := range []string{"/shared/supervisord/exec-in-ns", "/usr/local/bin/exec-in-ns"} {
		if pathExists(p) {
			return p
		}
	}
	return "exec-in-ns"
}
