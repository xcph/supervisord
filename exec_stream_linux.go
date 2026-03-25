//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	nodeagentv1 "github.com/xcph/cloudphone-nodeagent-api/pkg/apiv1"
	"github.com/ochinchina/supervisord/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
	}
	// Without -t, do not allocate PTY for sh/bash (isInteractiveShell); otherwise the shell waits on a
	// TTY and the gRPC stream (no stdin / pipe) looks hung. Plain unix.Exec exits when stdin is EOF.
	if !hs.GetTty() {
		cmd.Env = append(cmd.Env, "EXEC_IN_NS_NO_PTY=1")
	}

	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "start exec-in-ns: %v", err)
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
				ch := &nodeagentv1.ExecChunk{Chunk: &nodeagentv1.ExecChunk_Stderr{Stderr: append([]byte(nil), buf[:n]...)}}
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
	code, msg := exitFromWaitErr(w)
	if ce != nil {
		_ = stream.Send(&nodeagentv1.ExecChunk{Chunk: &nodeagentv1.ExecChunk_Done{Done: &nodeagentv1.ExecDone{ExitCode: -1, ErrorMessage: ce.Error()}}})
		return ce
	}
	_ = stream.Send(&nodeagentv1.ExecChunk{Chunk: &nodeagentv1.ExecChunk_Done{Done: &nodeagentv1.ExecDone{ExitCode: code, ErrorMessage: msg}}})
	return nil
}

// exitFromWaitErr maps cmd.Wait result to exit code; ECHILD/waitid noise is treated as success
// when pipes were already drained (common with namespace/exec edge cases).
func exitFromWaitErr(w error) (code int32, msg string) {
	if w == nil {
		return 0, ""
	}
	var exitErr *exec.ExitError
	if errors.As(w, &exitErr) {
		return int32(exitErr.ExitCode()), ""
	}
	if isWaitNoChildErr(w) {
		return 0, ""
	}
	return -1, w.Error()
}

func isWaitNoChildErr(w error) bool {
	s := w.Error()
	return strings.Contains(s, "no child processes") || strings.Contains(s, "waitid")
}
