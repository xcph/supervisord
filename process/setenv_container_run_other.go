//go:build !linux
// +build !linux

package process

func maybeInjectContainerRunAppArmorEnv(_ *Process) {}
