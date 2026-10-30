package process

import (
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestGetExitstatusReportsSignaledWhenStopped(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	dir := t.TempDir()
	e := writeProgramConf(t, dir, filepath.Join(dir, "bin"), 0)
	p := NewProcess("s", e)
	p.cmd = cmd
	p.state = Stopped
	want := 128 + int(syscall.SIGKILL)
	if got := p.GetExitstatus(); got != want {
		t.Fatalf("Stopped GetExitstatus=%d want %d", got, want)
	}
	p.state = Exited
	if got := p.GetExitstatus(); got != want {
		t.Fatalf("Exited GetExitstatus=%d want %d", got, want)
	}
	if got := p.GetStatus(); !strings.Contains(got, "exit status 137") {
		t.Fatalf("GetStatus=%q", got)
	}
	p.stopByUser = true
	p.stopTime = p.stopTime.UTC()
	desc := p.GetDescription()
	if !strings.Contains(desc, "exit status 137") || !strings.Contains(desc, "stopped by user") {
		t.Fatalf("GetDescription=%q", desc)
	}
}
