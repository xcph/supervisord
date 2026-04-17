//go:build linux
// +build linux

package util

import (
	"os"
	"strings"
)

const (
	appArmorEnabledPath   = "/sys/module/apparmor/parameters/enabled"
	appArmorModuleDir     = "/sys/module/apparmor"
	appArmorSecurityFS    = "/sys/kernel/security/apparmor"
	procSelfAttrExec      = "/proc/self/attr/exec"
)

func appArmorSubsystemPresent() bool {
	if _, err := os.Stat(appArmorSecurityFS); err == nil {
		return true
	}
	if _, err := os.Stat(appArmorModuleDir); err == nil {
		return true
	}
	return false
}

// AppArmorExecTransitionAvailable reports whether this task can use the kernel
// "exec" security attribute for an AppArmor profile transition (same path as
// aa_change_onexec / writes to /proc/self/attr/exec before execve).
//
// When false, supervisord should not inject SUPERVISORD_APPARMOR_* for container_run
// and __run_container__ / __run_gate_exec__ should skip prepareAppArmorExecTransition even if the env is set.
func AppArmorExecTransitionAvailable() bool {
	if !appArmorSubsystemPresent() {
		return false
	}
	if b, err := os.ReadFile(appArmorEnabledPath); err == nil && strings.TrimSpace(string(b)) == "N" {
		return false
	}
	f, err := os.OpenFile(procSelfAttrExec, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
