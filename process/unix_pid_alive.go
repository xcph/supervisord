//go:build !windows

package process

import (
	"errors"
	"syscall"
)

// unixPidAlive mirrors kill(pid, 0): EPERM means the PID exists but this process may not signal it
// (AppArmor/LSM, namespaces). Treat as alive so we do not false-trigger startsecs backoff or skip stray reap.
func unixPidAlive(pid int) bool {
	err := syscall.Kill(pid, syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
