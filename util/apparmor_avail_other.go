//go:build !linux
// +build !linux

package util

// AppArmorExecTransitionAvailable is always false on non-Linux builds.
func AppArmorExecTransitionAvailable() bool {
	return false
}
