// +build !linux

package main

import (
	"fmt"
	"os"
)

// runExecInNamespace is not implemented on non-Linux platforms.
func runExecInNamespace(_ int, _ []string, _, _, _ *os.File) error {
	return fmt.Errorf("exec subcommand is only supported on Linux")
}

// handleExecInNs is a no-op on non-Linux.
func handleExecInNs() bool {
	return false
}
