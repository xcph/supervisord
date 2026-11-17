//go:build linux

package process

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// RunNamespaceKeeper is PID 1 of a persisted container namespace.
// Supervisord re-execs itself with NamespaceKeeperMagic to enter it.
func RunNamespaceKeeper() {
	if err := prepareKeptNamespace(); err != nil {
		fmt.Fprintf(os.Stderr, "namespace keeper: %v\n", err)
		os.Exit(1)
	}
	f := os.NewFile(3, "keeper-ctl")
	conn, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "namespace keeper: control socket: %v\n", err)
		os.Exit(1)
	}
	if err := writeKeeperFrame(conn, keeperFrameReady, nil); err != nil {
		fmt.Fprintf(os.Stderr, "namespace keeper: ready: %v\n", err)
		os.Exit(1)
	}
	serveNamespaceKeeper(conn)
}

func prepareKeptNamespace() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		fmt.Fprintf(os.Stderr, "namespace keeper: mount / private: %v\n", err)
	}
	if err := unix.Unmount("/proc", unix.MNT_DETACH); err != nil && err != unix.ENOENT {
		fmt.Fprintf(os.Stderr, "namespace keeper: umount /proc: %v\n", err)
	}
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	return nil
}

func serveNamespaceKeeper(conn net.Conn) {
	defer conn.Close()
	var writeMu sync.Mutex
	send := func(kind byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeKeeperFrame(conn, kind, payload)
	}
	var mu sync.Mutex
	var childPid int
	reaped := map[int]int{}
	report := func(pid, code int) {
		var payload [8]byte
		binary.BigEndian.PutUint32(payload[:4], uint32(pid))
		binary.BigEndian.PutUint32(payload[4:], uint32(int32(code)))
		if err := send(keeperFrameExited, payload[:]); err != nil {
			os.Exit(1)
		}
	}
	go func() {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, 0, nil)
			if err != nil {
				if err == syscall.EINTR {
					continue
				}
				if err == syscall.ECHILD {
					time.Sleep(20 * time.Millisecond)
					continue
				}
				return
			}
			code := ReportExitCode(ws)
			mu.Lock()
			mine := pid == childPid && childPid != 0
			if mine {
				childPid = 0
			} else {
				reaped[pid] = code
			}
			mu.Unlock()
			if mine {
				report(pid, code)
			}
		}
	}()

	shutdown := func() {
		_ = syscall.Kill(-1, syscall.SIGKILL)
		os.Exit(0)
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	go func() {
		<-sigCh
		shutdown()
	}()

	for {
		kind, payload, err := readKeeperFrame(conn)
		if err != nil {
			shutdown()
		}
		switch kind {
		case keeperFrameShutdown:
			shutdown()
		case keeperFrameSignal:
			if len(payload) < 4 {
				continue
			}
			sig := syscall.Signal(binary.BigEndian.Uint32(payload[:4]))
			mu.Lock()
			pid := childPid
			mu.Unlock()
			if pid > 0 {
				_ = syscall.Kill(pid, sig)
			}
		case keeperFrameSpawn:
			dir, argv, env, err := decodeSpawn(payload)
			if err != nil {
				_ = send(keeperFrameError, []byte(err.Error()))
				continue
			}
			_ = syscall.Kill(-1, syscall.SIGKILL)
			pid, err := forkGate(dir, argv, env)
			if err != nil {
				_ = send(keeperFrameError, []byte(err.Error()))
				continue
			}
			var early *int
			mu.Lock()
			if code, ok := reaped[pid]; ok {
				c := code
				early = &c
				delete(reaped, pid)
			} else {
				childPid = pid
			}
			mu.Unlock()
			var body [4]byte
			binary.BigEndian.PutUint32(body[:], uint32(pid))
			if err := send(keeperFrameStarted, body[:]); err != nil {
				os.Exit(1)
			}
			if early != nil {
				report(pid, *early)
			}
		}
	}
}

func forkGate(dir string, argv, env []string) (int, error) {
	if len(argv) == 0 {
		return 0, fmt.Errorf("empty command")
	}
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = os.Args[0]
	}
	if len(env) == 0 {
		env = os.Environ()
	}
	gate := append([]string{exe, "__run_gate_exec__"}, argv...)
	return syscall.ForkExec(exe, gate, &syscall.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: []uintptr{0, 1, 2},
	})
}
