//go:build linux
// +build linux

package util

import "testing"

func TestAppArmorExecTransitionAvailableIsBoolean(t *testing.T) {
	// Smoke: must not panic; result depends on host kernel.
	_ = AppArmorExecTransitionAvailable()
}
