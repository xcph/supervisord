//go:build linux

package main

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestWaitProcessExitTimeoutKills(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid

	start := time.Now()
	ws, err := waitProcessExit(pid, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("waitProcessExit: %v", err)
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("want SIGKILL, got Signaled=%v Signal=%v", ws.Signaled(), ws.Signal())
	}
	if elapsed < 200*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("elapsed=%s, expected ~timeout then quick kill", elapsed)
	}
	// Child already reaped by waitProcessExit; cmd.Wait should observe that.
	_ = cmd.Wait()
}
