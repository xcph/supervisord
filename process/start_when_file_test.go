package process

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ochinchina/supervisord/config"
)

func writeProgramConf(t *testing.T, dir string, startFile string, timeout int) *config.Entry {
	t.Helper()
	iniPath := filepath.Join(dir, "supervisord.conf")
	content := `[program:testwf]
command=/bin/true
autostart=false
start_when_file=` + startFile + `
start_when_file_timeout=` + strconv.Itoa(timeout) + `
start_when_file_poll_secs=1
`
	if err := os.WriteFile(iniPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c := config.NewConfig(iniPath)
	if _, err := c.Load(); err != nil {
		t.Fatal(err)
	}
	e := c.GetProgram("testwf")
	if e == nil {
		t.Fatal("GetProgram(testwf) is nil")
	}
	return e
}

func TestWaitForStartFile_Disabled(t *testing.T) {
	e := config.NewEntry(".")
	e.Name = "program:x"
	p := NewProcess("s", e)
	if err := p.waitForStartFileIfConfigured(); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForStartFile_OK(t *testing.T) {
	dir := t.TempDir()
	okpath := filepath.Join(dir, "marker")
	if err := os.WriteFile(okpath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := writeProgramConf(t, dir, okpath, 5)
	p := NewProcess("s", e)
	if err := p.waitForStartFileIfConfigured(); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForStartFile_Timeout(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent")
	e := writeProgramConf(t, dir, missing, 2)
	p := NewProcess("s", e)
	err := p.waitForStartFileIfConfigured()
	if err == nil {
		t.Fatal("expected timeout")
	}
	if !strings.Contains(err.Error(), "timeout waiting for start_when_file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForStartFile_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	e := writeProgramConf(t, dir, sub, 1)
	p := NewProcess("s", e)
	err := p.waitForStartFileIfConfigured()
	if err == nil {
		t.Fatal("expected timeout (path is directory)")
	}
	if !strings.Contains(err.Error(), "timeout waiting for start_when_file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForStartFile_StopRequested(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent")
	e := writeProgramConf(t, dir, missing, 0)
	p := NewProcess("s", e)
	go func() {
		time.Sleep(200 * time.Millisecond)
		p.lock.Lock()
		p.stopByUser = true
		p.lock.Unlock()
	}()
	err := p.waitForStartFileIfConfigured()
	if !errors.Is(err, errStopRequestedDuringStartFileWait) {
		t.Fatalf("expected stop sentinel, got %v", err)
	}
}
