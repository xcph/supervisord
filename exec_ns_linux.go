// +build linux

package main

import (
	"os"
	"os/exec"
	"strconv"
)

// getExecInNsPath returns the path to the exec-in-ns binary (standalone, no /proc/self/exe).
func getExecInNsPath() string {
	if p := os.Getenv("EXEC_IN_NS_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, p := range []string{"/shared/supervisord/exec-in-ns", "/usr/local/bin/exec-in-ns"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "exec-in-ns"
}

// runExecInNamespace spawns exec-in-ns to join target's user/mnt/pid/net namespaces and exec the command.
// Uses standalone exec-in-ns binary to avoid supervisord's init chain that reads /proc/self/exe (fails in containers).
func runExecInNamespace(targetPid int, cmdArgs []string, stdin, stdout, stderr *os.File) error {
	execInNs := getExecInNsPath()
	args := append([]string{strconv.Itoa(targetPid)}, cmdArgs...)
	cmd := exec.Command(execInNs, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// handleExecInNs is unused when using exec-in-ns binary; kept for build.
func handleExecInNs() bool {
	return false
}
