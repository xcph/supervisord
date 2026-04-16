//go:build !linux

package process

// WaitForKoordletBeforeAndroidCpusetSetup is a no-op on non-Linux builds.
func WaitForKoordletBeforeAndroidCpusetSetup() {}
