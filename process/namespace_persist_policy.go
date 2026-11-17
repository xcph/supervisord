package process

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ochinchina/supervisord/signals"
	log "github.com/sirupsen/logrus"
)

// NamespaceKeeperMagic is the argv token that makes supervisord stay PID 1 of a
// container namespace and spawn later commands inside it.
const NamespaceKeeperMagic = "__run_namespace_keeper__"

// persistNamespaceDecision reports whether this program should keep one
// container namespace across restarts. The keeper is generic: it does not know
// which program it is hosting. Android init and smolvm stay one-namespace-per-start.
func persistNamespaceDecision(containerRun, persist bool, runType, command, goos string) (bool, string) {
	if goos != "linux" {
		return false, "not linux"
	}
	if !containerRun {
		return false, "container_run is off"
	}
	if !persist {
		return false, "container_run_persist is off"
	}
	if normalizeRunType(runType) == "smolvm" {
		return false, "smolvm"
	}
	if isInitNamespaceCommand(command) {
		return false, "init"
	}
	return true, ""
}

func isInitNamespaceCommand(command string) bool {
	cleaned := filepath.Clean(strings.TrimSpace(command))
	return cleaned == "/init" || cleaned == "/sbin/init"
}

func (p *Process) ensureNamespaceKeeper() error {
	if p.keeperAlive() {
		return nil
	}
	keeper, err := startNamespaceKeeper(getSupervisordPath(), p.StdoutLog, p.StderrLog, p.config)
	if err != nil {
		return err
	}
	p.keeper = keeper
	return nil
}

func (p *Process) stopNamespaceKeeper() {
	p.namespaceKept.Store(false)
	if p.keeper != nil {
		p.keeper.stop()
		p.keeper = nil
	}
	p.keptPid.Store(0)
}

func (p *Process) keeperAlive() bool {
	return p.keeper != nil && p.keeper.alive()
}

func (p *Process) spawnPersistedChild() error {
	if p.keeper == nil {
		return fmt.Errorf("namespace keeper is not running")
	}
	pid, err := p.keeper.spawn(p.cmd.Args, p.cmd.Env, p.cmd.Dir)
	if err != nil {
		return err
	}
	// Publish the new pid before clearing the previous exit. An older waiter
	// must observe this pid and leave it alone.
	p.keptPid.Store(int32(pid))
	p.keptExit.Store(-1)
	return nil
}

func (p *Process) waitPersistedChild() {
	pid := int(p.keptPid.Load())
	code := 1
	if p.keeper != nil && pid > 0 {
		if c, err := p.keeper.wait(pid); err == nil {
			code = c
		}
	}
	// A newer spawn may already own keptPid. Clearing it unconditionally
	// makes that live child look dead and restarts the FATAL loop.
	if pid > 0 && p.keptPid.CompareAndSwap(int32(pid), 0) {
		p.keptExit.Store(int32(code))
	}
	log.WithFields(log.Fields{"program": p.GetName(), "report_exit": code, "pid": pid}).Info("program stopped")
	p.lock.Lock()
	p.stopTime = time.Now()
	p.lock.Unlock()
}

func (p *Process) signalPersistedChild(sig os.Signal) error {
	if p.keeper == nil {
		return fmt.Errorf("namespace keeper is not running")
	}
	return p.keeper.signal(sig)
}

// Restart replaces the running program. With a live namespace keeper it only
// replaces the inner process, so ctl restart and config reloads keep the same
// PID namespace. Otherwise it is stop followed by start.
func (p *Process) Restart(wait bool) {
	if !p.keeperAlive() {
		p.Stop(true, false)
		p.Start(wait)
		return
	}
	old := p.GetPid()
	p.lock.Lock()
	p.explicitRestart = true
	p.lock.Unlock()
	sig, err := signals.ToSignal(p.config.GetString("stopsignal", "TERM"))
	if err != nil || sig == nil {
		sig = syscall.SIGTERM
	}
	if err := p.Signal(sig, false); err != nil {
		log.WithFields(log.Fields{"program": p.GetName(), "err": err}).Warn("restart signal failed")
	}
	if !wait {
		return
	}
	killAt := time.Now().Add(time.Duration(p.config.GetInt("stopwaitsecs", 10)) * time.Second)
	deadline := killAt.Add(time.Duration(p.config.GetInt("startsecs", 1)+30) * time.Second)
	killed := false
	for time.Now().Before(deadline) {
		st := p.GetState()
		pid := p.GetPid()
		if st == Fatal || st == Stopped {
			return
		}
		if st == Running && pid != 0 && pid != old {
			return
		}
		if !killed && time.Now().After(killAt) && pid == old && pid != 0 {
			_ = p.Signal(syscall.SIGKILL, false)
			killed = true
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (p *Process) persistNamespace(command string) (bool, string) {
	if p == nil || p.config == nil {
		return false, "no config"
	}
	if p.launchedSmolvm {
		return false, "smolvm"
	}
	runType := p.containerRunSpec().desired
	if p.smolvmFallback {
		runType = "namespace"
	}
	return persistNamespaceDecision(
		p.config.GetBool("container_run", false),
		p.config.GetBool("container_run_persist", false),
		runType,
		command,
		runtime.GOOS,
	)
}
