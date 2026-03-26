//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ochinchina/supervisord/types"
	nodeagentv1 "github.com/xcph/cloudphone-nodeagent-api/pkg/apiv1"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func debugEnabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func debugExecf(format string, args ...interface{}) {
	if debugEnabled(os.Getenv("SUPERVISORD_DEBUG_EXEC")) {
		fmt.Fprintf(os.Stderr, "[supervisord-exec] "+format+"\n", args...)
	}
}

func filterExecInNsNoise(in []byte) []byte {
	if len(in) == 0 {
		return in
	}
	s := strings.TrimSpace(string(in))
	if strings.HasPrefix(s, "exec-in-ns: exit status ") {
		// Old exec-in-ns binaries may print wrapped ExitError lines as stderr noise.
		// Suppress these to keep client-side exit UX clean.
		return nil
	}
	return in
}

const execCopyChunk = 32 * 1024

func (t *tunnelServer) ExecStream(stream grpc.BidiStreamingServer[nodeagentv1.ExecChunk, nodeagentv1.ExecChunk]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hs := first.GetHandshake()
	if hs == nil {
		return status.Error(codes.InvalidArgument, "first message must be ExecHandshake")
	}
	debugExecf("handshake program=%s tty=%v stdin=%v rows=%d cols=%d argv=%v",
		hs.ProgramName, hs.Tty, hs.Stdin, hs.TermRows, hs.TermCols, hs.Argv)
	if hs.ProgramName == "" {
		return status.Error(codes.InvalidArgument, "handshake.program_name is required")
	}

	var reply struct{ ProcInfo types.ProcessInfo }
	if err := t.sup.GetProcessInfo(nil, &struct{ Name string }{Name: hs.ProgramName}, &reply); err != nil {
		return status.Errorf(codes.NotFound, "program %q: %v", hs.ProgramName, err)
	}
	if reply.ProcInfo.Pid == 0 {
		return status.Errorf(codes.FailedPrecondition, "program %q is not running", hs.ProgramName)
	}

	cmdArgs := append([]string(nil), hs.Argv...)
	if len(cmdArgs) == 0 {
		if hs.Tty {
			shell := os.Getenv("SHELL")
			if shell == "" {
				shell = "/bin/bash"
			}
			cmdArgs = []string{shell}
		} else {
			return status.Error(codes.InvalidArgument, "argv empty: use tty for interactive shell or pass a command after --")
		}
	}

	execInNs := getExecInNsPath()
	args := append([]string{strconv.Itoa(reply.ProcInfo.Pid)}, cmdArgs...)
	cmd := exec.Command(execInNs, args...)
	var resizePipePath string
	var resizePipeFile *os.File

	var stdinW *io.PipeWriter
	if hs.Stdin {
		stdinR, w := io.Pipe()
		stdinW = w
		cmd.Stdin = stdinR
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stdout pipe: %v", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stderr pipe: %v", err)
	}

	cmd.Env = os.Environ()
	if hs.GetTty() && hs.GetTermRows() > 0 && hs.GetTermCols() > 0 {
		cmd.Env = append(cmd.Env,
			"EXEC_IN_NS_ROWS="+strconv.FormatUint(uint64(hs.GetTermRows()), 10),
			"EXEC_IN_NS_COLS="+strconv.FormatUint(uint64(hs.GetTermCols()), 10),
		)
		// Dynamic PTY resize channel: supervisord writes "rows cols\n" to this FIFO.
		// Some images may not have /tmp, so try multiple writable directories.
		candidates := []string{os.TempDir(), "/tmp", "/dev/shm", "."}
		for _, dir := range candidates {
			dir = strings.TrimSpace(dir)
			if dir == "" {
				continue
			}
			if err := os.MkdirAll(dir, 0755); err != nil {
				debugExecf("mkfifo skip dir=%s mkdir err=%v", dir, err)
				continue
			}
			path := filepath.Join(dir, "exec-in-ns-resize-"+strconv.FormatInt(time.Now().UnixNano(), 10))
			_ = os.Remove(path)
			ferr := unix.Mkfifo(path, 0600)
			if ferr == nil {
				resizePipePath = path
				cmd.Env = append(cmd.Env, "EXEC_IN_NS_RESIZE_FIFO="+resizePipePath)
				debugExecf("mkfifo ok path=%s", resizePipePath)
				break
			}
			debugExecf("mkfifo failed path=%s err=%v", path, ferr)
		}
		if resizePipePath == "" {
			debugExecf("mkfifo disabled: no usable directory for resize fifo")
		}
	}
	// Without -t, do not allocate PTY for sh/bash (isInteractiveShell); otherwise the shell waits on a
	// TTY and the gRPC stream (no stdin / pipe) looks hung. Plain unix.Exec exits when stdin is EOF.
	if !hs.GetTty() {
		cmd.Env = append(cmd.Env, "EXEC_IN_NS_NO_PTY=1")
	}

	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "start exec-in-ns: %v", err)
	}
	if resizePipePath != "" {
		if f, ferr := os.OpenFile(resizePipePath, os.O_RDWR, 0600); ferr == nil {
			resizePipeFile = f
			defer resizePipeFile.Close()
			debugExecf("opened resize fifo path=%s", resizePipePath)
		}
		defer os.Remove(resizePipePath)
	}

	var copyMu sync.Mutex
	var copyErr error
	setCopyErr := func(e error) {
		if e == nil || errors.Is(e, io.EOF) {
			return
		}
		copyMu.Lock()
		if copyErr == nil {
			copyErr = e
		}
		copyMu.Unlock()
	}

	var wg sync.WaitGroup
	var stderrTailMu sync.Mutex
	var stderrTail bytes.Buffer
	appendStderrTail := func(p []byte) {
		if len(p) == 0 {
			return
		}
		stderrTailMu.Lock()
		defer stderrTailMu.Unlock()
		_, _ = stderrTail.Write(p)
		const maxTail = 8 * 1024
		if stderrTail.Len() > maxTail {
			b := stderrTail.Bytes()
			keep := b[len(b)-maxTail:]
			stderrTail.Reset()
			_, _ = stderrTail.Write(keep)
		}
	}
	getStderrTail := func() string {
		stderrTailMu.Lock()
		defer stderrTailMu.Unlock()
		return stderrTail.String()
	}

	if hs.Stdin && stdinW != nil {
		w := stdinW
		go func() {
			defer w.Close()
			for {
				m, rerr := stream.Recv()
				if rerr != nil {
					setCopyErr(rerr)
					return
				}
				data := m.GetStdin()
				if rz := m.GetResize(); rz != nil {
					if resizePipeFile != nil && rz.TermRows > 0 && rz.TermCols > 0 {
						_, werr := io.WriteString(resizePipeFile, strconv.FormatUint(uint64(rz.TermRows), 10)+" "+strconv.FormatUint(uint64(rz.TermCols), 10)+"\n")
						debugExecf("recv resize rows=%d cols=%d write_err=%v", rz.TermRows, rz.TermCols, werr)
					}
					continue
				}
				if len(data) == 0 {
					continue
				}
				if _, werr := w.Write(data); werr != nil {
					setCopyErr(werr)
					return
				}
			}
		}()
	} else {
		// Client half-closes after handshake (no stdin). We must still Recv until EOF or gRPC can
		// block outbound Send (HTTP/2 flow control), and stdout never reaches the client.
		go func() {
			for {
				_, rerr := stream.Recv()
				if rerr != nil {
					if rerr != io.EOF {
						setCopyErr(rerr)
					}
					return
				}
			}
		}()
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, execCopyChunk)
		for {
			n, rerr := stdoutPipe.Read(buf)
			if n > 0 {
				ch := &nodeagentv1.ExecChunk{Chunk: &nodeagentv1.ExecChunk_Stdout{Stdout: append([]byte(nil), buf[:n]...)}}
				if serr := stream.Send(ch); serr != nil {
					setCopyErr(serr)
					return
				}
			}
			if rerr == io.EOF {
				return
			}
			if rerr != nil {
				setCopyErr(rerr)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, execCopyChunk)
		for {
			n, rerr := stderrPipe.Read(buf)
			if n > 0 {
				appendStderrTail(buf[:n])
				filtered := filterExecInNsNoise(buf[:n])
				if len(filtered) == 0 {
					if rerr == io.EOF {
						return
					}
					if rerr != nil {
						setCopyErr(rerr)
						return
					}
					continue
				}
				ch := &nodeagentv1.ExecChunk{Chunk: &nodeagentv1.ExecChunk_Stderr{Stderr: append([]byte(nil), filtered...)}}
				if serr := stream.Send(ch); serr != nil {
					setCopyErr(serr)
					return
				}
			}
			if rerr == io.EOF {
				return
			}
			if rerr != nil {
				setCopyErr(rerr)
				return
			}
		}
	}()

	wg.Wait()

	copyMu.Lock()
	ce := copyErr
	copyMu.Unlock()

	if ce != nil {
		if errors.Is(ce, io.EOF) {
			return nil
		}
		_ = cmd.Process.Kill()
	}

	w := cmd.Wait()
	code, msg := exitFromWaitErr(cmd, w)
	// Compatibility fallback for older exec-in-ns binaries that return 1 on
	// command-not-found, while shell semantics expect 127.
	if code == 1 {
		tail := strings.ToLower(getStderrTail())
		if strings.Contains(tail, "executable file not found in $path") ||
			strings.Contains(tail, "executable file not found in \\$path") {
			code = 127
		}
	}
	if ce != nil {
		_ = stream.Send(&nodeagentv1.ExecChunk{Chunk: &nodeagentv1.ExecChunk_Done{Done: &nodeagentv1.ExecDone{ExitCode: -1, ErrorMessage: ce.Error()}}})
		return ce
	}
	_ = stream.Send(&nodeagentv1.ExecChunk{Chunk: &nodeagentv1.ExecChunk_Done{Done: &nodeagentv1.ExecDone{ExitCode: code, ErrorMessage: msg}}})
	return nil
}

// exitFromWaitErr maps cmd.Wait result to exit code.
// Prefer ProcessState exit code when available to avoid waitid race noise.
func exitFromWaitErr(cmd *exec.Cmd, w error) (code int32, msg string) {
	if cmd != nil && cmd.ProcessState != nil {
		if ec := cmd.ProcessState.ExitCode(); ec >= 0 {
			return int32(ec), ""
		}
	}
	if w == nil {
		return 0, ""
	}
	if isWaitNoChildErr(w) {
		// Rare race: wait reports no child although process already exited.
		// Try to infer a deterministic code for common shell-form command.
		if ec, ok := inferExitCodeFromCmdArgs(cmd); ok {
			return int32(ec), ""
		}
	}
	var exitErr *exec.ExitError
	if errors.As(w, &exitErr) {
		return int32(exitErr.ExitCode()), ""
	}
	return -1, w.Error()
}

func isWaitNoChildErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "waitid") || strings.Contains(s, "no child processes")
}

func inferExitCodeFromCmdArgs(cmd *exec.Cmd) (int, bool) {
	if cmd == nil || len(cmd.Args) < 2 {
		return 0, false
	}
	for i := 0; i+1 < len(cmd.Args); i++ {
		if cmd.Args[i] == "-c" {
			script := strings.TrimSpace(cmd.Args[i+1])
			fields := strings.Fields(script)
			if len(fields) == 2 && fields[0] == "exit" {
				n, err := strconv.Atoi(fields[1])
				if err == nil && n >= 0 && n <= 255 {
					return n, true
				}
			}
		}
	}
	return 0, false
}
