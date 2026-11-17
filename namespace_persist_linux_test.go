//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ochinchina/supervisord/process"
)

func TestMain(m *testing.M) {
	if process.MaybeRunNamespaceKeeper(os.Args) {
		return
	}
	if handleRunGateExec() {
		return
	}
	os.Exit(m.Run())
}

func TestPersistedNamespaceKeepsInode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("CLONE_NEWPID requires root in this test")
	}
	keeper, err := process.StartNamespaceKeeper(os.Args[0], nil, os.Stderr, nil)
	if err != nil {
		t.Skipf("keeper did not start: %v", err)
	}
	defer keeper.Stop()

	first := readNamespaceLink(t, keeper)
	second := readNamespaceLink(t, keeper)
	if first == "" || first != second {
		t.Fatalf("inode changed across spawn: %q %q", first, second)
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "leftover.pid")
	code := runInKeeper(t, keeper, "/bin/sh", "-c", "sleep 60 & echo $! > "+shellQuote(pidFile))
	if code != 0 {
		t.Fatalf("leftover spawn exit %d", code)
	}
	code = runInKeeper(t, keeper, "/bin/sh", "-c", "pid=$(cat "+shellQuote(pidFile)+"); if kill -0 \"$pid\" 2>/dev/null; then echo alive; exit 2; else exit 0; fi")
	if code != 0 {
		t.Fatalf("leftover process still alive, exit %d", code)
	}

	code = runInKeeper(t, keeper, "/bin/sh", "-c", "exit 3")
	if code != 3 {
		t.Fatalf("exit code %d want 3", code)
	}

	keeper.Stop()
	next, err := process.StartNamespaceKeeper(os.Args[0], nil, os.Stderr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Stop()
	third := readNamespaceLink(t, next)
	if third == first {
		t.Fatalf("new keeper reused inode %q", third)
	}
}

func readNamespaceLink(t *testing.T, keeper *process.NamespaceKeeper) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "ns")
	code := runInKeeper(t, keeper, "/bin/sh", "-c", "readlink /proc/self/ns/pid > "+shellQuote(out))
	if code != 0 {
		t.Fatalf("readlink exit %d", code)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(body))
}

func runInKeeper(t *testing.T, keeper *process.NamespaceKeeper, argv ...string) int {
	t.Helper()
	if _, err := exec.LookPath(argv[0]); err != nil && argv[0] == "/bin/sh" {
		t.Fatal(err)
	}
	pid, err := keeper.Spawn(argv, os.Environ(), "")
	if err != nil {
		t.Fatal(err)
	}
	code, err := keeper.Wait(pid)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}
