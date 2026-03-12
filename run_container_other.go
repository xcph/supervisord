// +build !linux

package main

// handleRunContainer is a no-op on non-Linux (container run is Linux-only).
func handleRunContainer() bool {
	return false
}
