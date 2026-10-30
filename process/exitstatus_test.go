package process

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestReportExitCodeExited(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 78")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-nil exit error")
	}
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Exited() || ws.ExitStatus() != 78 {
		t.Fatalf("fixture: ok=%v Exited=%v ExitStatus=%d", ok, ws.Exited(), ws.ExitStatus())
	}
	if got := ReportExitCode(ws); got != 78 {
		t.Fatalf("ReportExitCode=%d want 78", got)
	}
}

func TestFormatExitDescriptionExited(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 78")
	_ = cmd.Run()
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	got := FormatExitDescription(ws)
	if got != "exit status 78" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatExitDescriptionSignaled(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	got := FormatExitDescription(ws)
	wantSub := "exit status 137 (signal"
	if !strings.Contains(got, wantSub) {
		t.Fatalf("FormatExitDescription=%q want substring %q", got, wantSub)
	}
	if !WaitFinished(ws) {
		t.Fatal("WaitFinished should be true")
	}
}

func TestReportExitCodeSignaled(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("fixture: ok=%v Signaled=%v Signal=%v", ok, ws.Signaled(), ws.Signal())
	}
	want := 128 + int(syscall.SIGKILL)
	if got := ReportExitCode(ws); got != want {
		t.Fatalf("ReportExitCode=%d want %d (raw ExitStatus=%d)", got, want, ws.ExitStatus())
	}
	// The bug we fix: ExitStatus() alone is not a usable report code for signaled waits.
	if ws.ExitStatus() == want {
		t.Fatalf("ExitStatus() unexpectedly equals report code; fixture may be wrong")
	}
}
