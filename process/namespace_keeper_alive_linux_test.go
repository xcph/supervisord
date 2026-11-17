//go:build linux

package process

import (
	"os/exec"
	"testing"
)

func TestPersistedChildAliveDoesNotProbeHostPID(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// A PID that only exists inside the container namespace. It must not be
	// resolved with kill(2) from the supervisor's namespace.
	const namespaceLocalPID = int32(1<<30 - 1)
	if unixPidAlive(int(namespaceLocalPID)) {
		t.Skip("host already has this pid")
	}

	p := &Process{keeper: &NamespaceKeeper{cmd: cmd, dead: make(chan struct{})}}
	p.keptPid.Store(namespaceLocalPID)
	if !p.persistedChildAlive() {
		t.Fatal("child must stay alive until the keeper reports exit")
	}

	p.keptPid.Store(0)
	if p.persistedChildAlive() {
		t.Fatal("cleared child pid must be dead")
	}

	if got := p.NamespaceHostPid(); got != cmd.Process.Pid {
		t.Fatalf("host pid=%d want keeper %d", got, cmd.Process.Pid)
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	p.keptPid.Store(namespaceLocalPID)
	if p.persistedChildAlive() {
		t.Fatal("dead keeper must not keep the child alive")
	}
	if got := p.NamespaceHostPid(); got != 0 {
		t.Fatalf("dead keeper host pid=%d", got)
	}
}
