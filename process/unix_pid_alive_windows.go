//go:build windows

package process

// unixPidAlive is unused on Windows (isRunning uses ps; reap uses Process.Signal).
func unixPidAlive(pid int) bool {
	_ = pid
	return false
}
