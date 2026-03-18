//go:build !linux
// +build !linux

package main

import "runtime"

// getNproc 返回当前进程可用的 CPU 数量（非 Linux 平台使用 runtime.NumCPU）。
func getNproc() int {
	return runtime.NumCPU()
}
