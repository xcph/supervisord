// +build !linux

package main

// handleRunHelper is a no-op on non-Linux.
func handleRunHelper() bool {
	return false
}

// handleRunContainer is a no-op on non-Linux (container run is Linux-only).
func handleRunContainer() bool {
	return false
}
