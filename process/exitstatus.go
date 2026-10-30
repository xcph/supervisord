package process

import (
	"fmt"
	"syscall"
)

// ReportExitCode maps a wait(2) status to the shell-style exit code supervisord
// uses for exitcodes / autorestart / XML-RPC exitstatus.
//
//	Exited(n)    → n
//	Signaled(s)  → 128+s (capped at 255)
//	otherwise    → 1
//
// __run_container__ reaper calls ReportExitCode before os.Exit so outer
// Process decisions match the wrapper's reported status.
func ReportExitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		c := 128 + int(ws.Signal())
		if c > 255 {
			return 255
		}
		return c
	}
	if ws.Exited() {
		return ws.ExitStatus()
	}
	return 1
}

// FormatExitDescription is a human-readable one-line summary for ctl / XML-RPC
// description fields (includes shell exit code and signal name when applicable).
func FormatExitDescription(ws syscall.WaitStatus) string {
	code := ReportExitCode(ws)
	var s string
	switch {
	case ws.Signaled():
		s = fmt.Sprintf("exit status %d (signal %v)", code, ws.Signal())
	case ws.Exited():
		s = fmt.Sprintf("exit status %d", code)
	default:
		s = fmt.Sprintf("exit status %d", code)
	}
	if ws.CoreDump() {
		s += "; core dumped"
	}
	return s
}

// WaitFinished reports whether ws represents a finished process (exited or signaled).
func WaitFinished(ws syscall.WaitStatus) bool {
	return ws.Exited() || ws.Signaled()
}
