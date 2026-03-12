// +build linux

package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/ochinchina/supervisord/config"
)

// CLONE_NEWCGROUP creates a new cgroup namespace (Linux 4.6+).
const CLONE_NEWCGROUP = 0x02000000

const runContainerMagic = "__run_container__"

// setContainerRun configures the process to run in an isolated container (runc-like)
// when container_run=true: PID, mount, cgroup, UTS, IPC. Network ns excluded by default
// (container_network_isolated=false) because redroid netd needs pod network for iptables.
func setContainerRun(sysProcAttr *syscall.SysProcAttr, entry *config.Entry) {
	if !entry.GetBool("container_run", false) {
		return
	}
	sysProcAttr.Cloneflags |= syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | CLONE_NEWCGROUP |
		syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC
	if entry.GetBool("container_network_isolated", false) {
		sysProcAttr.Cloneflags |= syscall.CLONE_NEWNET
	}
}

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

// createContainerRunWrapper wraps the command to run via supervisord __run_container__.
func createContainerRunWrapper(args []string) []string {
	if len(args) == 0 {
		return args
	}
	return append([]string{getSupervisordPath(), runContainerMagic}, args...)
}
