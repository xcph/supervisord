//go:build linux

package process

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/ochinchina/supervisord/config"
	"golang.org/x/sys/unix"
)

// NamespaceKeeper is PID 1 of one container namespace. The supervisor stays
// outside and asks it to spawn, signal, and collect the program process.
type NamespaceKeeper struct {
	cmd  *exec.Cmd
	conn net.Conn
	mu   sync.Mutex

	ready     chan struct{}
	started   chan int
	spawnErr  chan error
	exited    chan keeperExit
	dead      chan struct{}
	closeDead sync.Once
	stopped   sync.Once
}

type keeperExit struct {
	pid  int
	code int
}

func startNamespaceKeeper(bin string, stdout, stderr io.Writer, entry *config.Entry) (*NamespaceKeeper, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	parent := os.NewFile(uintptr(fds[0]), "keeper-parent")
	child := os.NewFile(uintptr(fds[1]), "keeper-child")
	defer child.Close()

	cmd := exec.Command(bin, NamespaceKeeperMagic)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setPersistedKeeperClone(cmd.SysProcAttr, entry)
	cmd.ExtraFiles = []*os.File{child}
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if stderr != nil {
		cmd.Stderr = stderr
	}
	joinForegroundCpusetBeforeFork()
	if err := cmd.Start(); err != nil {
		parent.Close()
		return nil, err
	}
	conn, err := net.FileConn(parent)
	parent.Close()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	k := &NamespaceKeeper{
		cmd:      cmd,
		conn:     conn,
		ready:    make(chan struct{}),
		started:  make(chan int, 1),
		spawnErr: make(chan error, 1),
		exited:   make(chan keeperExit, 32),
		dead:     make(chan struct{}),
	}
	go k.readLoop()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-k.ready:
		return k, nil
	case <-k.dead:
		_ = cmd.Wait()
		return nil, fmt.Errorf("namespace keeper exited before ready")
	case <-timer.C:
		k.stop()
		return nil, fmt.Errorf("namespace keeper did not become ready")
	}
}

func setPersistedKeeperClone(attr *syscall.SysProcAttr, entry *config.Entry) {
	attr.Cloneflags |= syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | CLONE_NEWCGROUP | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC
	if entry != nil && entry.GetBool("container_network_isolated", false) {
		attr.Cloneflags |= syscall.CLONE_NEWNET
	}
}

func (k *NamespaceKeeper) readLoop() {
	defer k.closeDead.Do(func() { close(k.dead) })
	ready := false
	markReady := func() {
		if !ready {
			ready = true
			close(k.ready)
		}
	}
	for {
		kind, payload, err := readKeeperFrame(k.conn)
		if err != nil {
			return
		}
		switch kind {
		case keeperFrameReady:
			markReady()
		case keeperFrameStarted:
			if len(payload) < 4 {
				continue
			}
			k.started <- int(binary.BigEndian.Uint32(payload[:4]))
		case keeperFrameExited:
			if len(payload) < 8 {
				continue
			}
			k.exited <- keeperExit{
				pid:  int(binary.BigEndian.Uint32(payload[:4])),
				code: int(int32(binary.BigEndian.Uint32(payload[4:8]))),
			}
		case keeperFrameError:
			k.spawnErr <- fmt.Errorf("namespace keeper: %s", payload)
			markReady()
		}
	}
}

func (k *NamespaceKeeper) spawn(argv, env []string, dir string) (int, error) {
	payload, err := encodeSpawn(dir, argv, env)
	if err != nil {
		return 0, err
	}
	k.mu.Lock()
	err = writeKeeperFrame(k.conn, keeperFrameSpawn, payload)
	k.mu.Unlock()
	if err != nil {
		return 0, err
	}
	select {
	case pid := <-k.started:
		return pid, nil
	case err := <-k.spawnErr:
		return 0, err
	case <-k.dead:
		return 0, fmt.Errorf("namespace keeper exited")
	}
}

func (k *NamespaceKeeper) signal(sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported signal %v", sig)
	}
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(s))
	k.mu.Lock()
	defer k.mu.Unlock()
	return writeKeeperFrame(k.conn, keeperFrameSignal, payload[:])
}

func (k *NamespaceKeeper) wait(pid int) (int, error) {
	for {
		select {
		case <-k.dead:
			return 1, fmt.Errorf("namespace keeper exited")
		case exit := <-k.exited:
			if pid == 0 || exit.pid == pid {
				return exit.code, nil
			}
		}
	}
}

func (k *NamespaceKeeper) stop() {
	if k == nil {
		return
	}
	k.stopped.Do(func() {
		k.mu.Lock()
		_ = writeKeeperFrame(k.conn, keeperFrameShutdown, nil)
		k.mu.Unlock()
		done := make(chan struct{})
		go func() {
			if k.cmd != nil {
				_ = k.cmd.Wait()
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			if k.cmd != nil && k.cmd.Process != nil {
				_ = k.cmd.Process.Kill()
			}
			<-done
		}
		k.closeDead.Do(func() { close(k.dead) })
		if k.conn != nil {
			_ = k.conn.Close()
		}
	})
}

func (p *Process) persistedChildAlive() bool {
	pid := int(p.keptPid.Load())
	return pid > 0 && unixPidAlive(pid)
}

// StartNamespaceKeeper starts a PID 1 for tests and for a persisted program.
func StartNamespaceKeeper(bin string, stdout, stderr io.Writer, entry *config.Entry) (*NamespaceKeeper, error) {
	return startNamespaceKeeper(bin, stdout, stderr, entry)
}

func (k *NamespaceKeeper) Spawn(argv, env []string, dir string) (int, error) {
	return k.spawn(argv, env, dir)
}

func (k *NamespaceKeeper) Wait(pid int) (int, error) { return k.wait(pid) }

func (k *NamespaceKeeper) Stop() { k.stop() }

func MaybeRunNamespaceKeeper(args []string) bool {
	if len(args) > 1 && args[1] == NamespaceKeeperMagic {
		RunNamespaceKeeper()
		return true
	}
	return false
}

func (k *NamespaceKeeper) alive() bool {
	if k == nil || k.cmd == nil || k.cmd.Process == nil {
		return false
	}
	select {
	case <-k.dead:
		return false
	default:
		return unixPidAlive(k.cmd.Process.Pid)
	}
}
