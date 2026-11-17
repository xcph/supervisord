package process

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mitchellh/go-ps"
	"github.com/ochinchina/filechangemonitor"
	"github.com/ochinchina/supervisord/config"
	"github.com/ochinchina/supervisord/events"
	"github.com/ochinchina/supervisord/logger"
	"github.com/ochinchina/supervisord/signals"
	"github.com/robfig/cron/v3"
	log "github.com/sirupsen/logrus"
)

// errStopRequestedDuringStartFileWait is returned when stopByUser becomes true while waiting on start_when_file.
var errStopRequestedDuringStartFileWait = errors.New("stop requested while waiting for start_when_file")

// startDrainAfterStop* — When Stop returns while a prior Start/run goroutine still holds inStart=true
// (e.g. REST stop with Wait:false), the next Start must wait for that goroutine to finish draining.
const (
	startDrainAfterStopPoll         = 50 * time.Millisecond
	startDrainAfterStopMax          = 90 * time.Second
	startDrainAfterStopImmediateMax = 15 * time.Second
)

// State the state of process
type State int

const (
	// Stopped the stopped state
	Stopped State = iota

	// Starting the starting state
	Starting = 10

	// Running the running state
	Running = 20

	// Backoff the backoff state
	Backoff = 30

	// Stopping the stopping state
	Stopping = 40

	// Exited the Exited state
	Exited = 100

	// Fatal the Fatal state
	Fatal = 200

	// Unknown the unknown state
	Unknown = 1000
)

var scheduler *cron.Cron = nil

func init() {
	scheduler = cron.New(cron.WithSeconds())
	scheduler.Start()
}

// String convert State to human-readable string
func (p State) String() string {
	switch p {
	case Stopped:
		return "Stopped"
	case Starting:
		return "Starting"
	case Running:
		return "Running"
	case Backoff:
		return "Backoff"
	case Stopping:
		return "Stopping"
	case Exited:
		return "Exited"
	case Fatal:
		return "Fatal"
	default:
		return "Unknown"
	}
}

// Process the program process management data
type Process struct {
	supervisorID string
	config       *config.Entry
	cmd          *exec.Cmd
	startTime    time.Time
	stopTime     time.Time
	state        State
	// true if process is starting
	inStart bool
	// true if the process is stopped by user
	stopByUser bool
	// set when Stop(immediate=true) runs; Start() uses a shorter inStart drain window.
	lastImmediateStop time.Time
	retryTimes        *int32
	lock              sync.RWMutex
	stdin             io.WriteCloser
	StdoutLog         logger.Logger
	StderrLog         logger.Logger
	// smolvmFallback is set after the virtual machine fails once in this start.
	smolvmFallback       bool
	smolvmFallbackReason string
	launchedSmolvm       bool
	relayStop            chan struct{}
	// namespaceKept reuses one PID namespace across restarts of this program.
	namespaceKept atomic.Bool
	keptPid       atomic.Int32
	keptExit      atomic.Int32
	// explicitRestart asks the start loop to spawn again without destroying the namespace.
	explicitRestart bool
	keeper          *NamespaceKeeper
}

// NewProcess creates new Process object
func NewProcess(supervisorID string, config *config.Entry) *Process {
	proc := &Process{supervisorID: supervisorID,
		config:     config,
		cmd:        nil,
		startTime:  time.Unix(0, 0),
		stopTime:   time.Unix(0, 0),
		state:      Stopped,
		inStart:    false,
		stopByUser: false,
		retryTimes: new(int32)}
	proc.keptExit.Store(-1)
	proc.config = config
	proc.cmd = nil
	proc.addToCron()
	return proc
}

func (p *Process) GetConfig() *config.Entry {
	return p.config
}

// add this process to crontab
func (p *Process) addToCron() {
	s := p.config.GetString("cron", "")

	if s != "" {
		log.WithFields(log.Fields{"program": p.GetName()}).Info("try to create cron program with cron expression:", s)
		scheduler.AddFunc(s, func() {
			log.WithFields(log.Fields{"program": p.GetName()}).Info("start cron program")
			if !p.isRunning() {
				p.Start(false)
			}
		})
	}

}

// Start process
// Args:
//
//	wait - true, wait the program started or failed
func (p *Process) Start(wait bool) {
	log.WithFields(log.Fields{"program": p.GetName()}).Info("try to start program")
	p.lock.RLock()
	drainMax := startDrainAfterStopMax
	if !p.lastImmediateStop.IsZero() && time.Since(p.lastImmediateStop) < 2*time.Minute {
		drainMax = startDrainAfterStopImmediateMax
	}
	p.lock.RUnlock()
	deadline := time.Now().Add(drainMax)
	for {
		p.lock.Lock()
		if !p.inStart {
			// 必须在 Unlock 之前置位，否则并发两次 Start() 可能都通过 inStart 检查，拉起重复子进程。
			p.inStart = true
			p.stopByUser = false
			p.lock.Unlock()
			break
		}
		drainingAfterStop := p.stopByUser
		p.lock.Unlock()
		if drainingAfterStop {
			if time.Now().After(deadline) {
				log.WithFields(log.Fields{"program": p.GetName()}).Warn(
					"Start: timeout waiting for prior run to drain after stop (inStart still true)")
				return
			}
			time.Sleep(startDrainAfterStopPoll)
			continue
		}
		log.WithFields(log.Fields{"program": p.GetName()}).Info("Don't start program again, program is already started")
		return
	}

	var runCond *sync.Cond
	if wait {
		runCond = sync.NewCond(&sync.Mutex{})
		runCond.L.Lock()
	}

	go func() {
		for {
			// we'll do retry start if it sets.
			p.run(func() {
				if wait {
					runCond.L.Lock()
					runCond.Signal()
					runCond.L.Unlock()
				}
			})
			// avoid print too many logs if fail to start program too quickly
			if time.Now().Unix()-p.startTime.Unix() < 2 {
				time.Sleep(5 * time.Second)
			}
			p.lock.Lock()
			explicit := p.explicitRestart
			p.explicitRestart = false
			stopped := p.stopByUser
			p.lock.Unlock()
			if explicit && !stopped {
				continue
			}
			if p.GetState() == Fatal {
				log.WithFields(log.Fields{"program": p.GetName()}).Info("program entered Fatal, don't start it again")
				break
			}
			if stopped {
				log.WithFields(log.Fields{"program": p.GetName()}).Info("program stopped by user, don't start it again")
				break
			}
			if !p.isAutoRestart() {
				log.WithFields(log.Fields{"program": p.GetName()}).Info("Don't start the stopped program because its autorestart flag is false")
				break
			}
		}
		// FATAL leaves the keeper up so a later shell or doctor --fix still
		// joins the gateway PID namespace. A user stop or an expected exit
		// that will not restart drops it.
		if p.GetState() != Fatal {
			p.stopNamespaceKeeper()
		}
		p.lock.Lock()
		p.inStart = false
		p.lock.Unlock()
	}()

	if wait {
		runCond.Wait()
		runCond.L.Unlock()
	}
}

// GetName returns name of program or event listener
func (p *Process) GetName() string {
	if p.config.IsProgram() {
		return p.config.GetProgramName()
	} else if p.config.IsEventListener() {
		return p.config.GetEventListenerName()
	} else {
		return ""
	}
}

// GetGroup returns group the program belongs to
func (p *Process) GetGroup() string {
	return p.config.Group
}

// GetDescription returns process status description for ctl status / XML-RPC.
// Running: uptime. Terminal / stopping: exit status (and signal), expectedness, stop reason.
func (p *Process) GetDescription() string {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if p.state == Running {
		if p.cmd == nil || p.cmd.Process == nil {
			return "running"
		}
		seconds := int(time.Now().Sub(p.startTime).Seconds())
		minutes := seconds / 60
		hours := minutes / 60
		days := hours / 24
		if days > 0 {
			return fmt.Sprintf("pid %d, uptime %d days, %d:%02d:%02d", p.cmd.Process.Pid, days, hours%24, minutes%60, seconds%60)
		}
		return fmt.Sprintf("pid %d, uptime %d:%02d:%02d", p.cmd.Process.Pid, hours%24, minutes%60, seconds%60)
	}

	var parts []string
	switch p.state {
	case Exited, Backoff, Stopped, Fatal, Stopping:
		if p.cmd != nil && p.cmd.ProcessState != nil {
			if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && WaitFinished(ws) {
				parts = append(parts, FormatExitDescription(ws))
				code := ReportExitCode(ws)
				if p.inExitCodes(code) {
					parts = append(parts, "expected")
				} else if p.state == Exited || p.state == Backoff || p.state == Fatal {
					parts = append(parts, "not expected")
				}
			}
		}
		if p.stopByUser {
			parts = append(parts, "stopped by user")
		}
		if p.stopTime.Unix() > 0 {
			parts = append(parts, p.stopTime.UTC().Format(time.RFC3339))
		}
	}
	return strings.Join(parts, "; ")
}

// GetExitstatus returns the shell-style exit code of the process after it has left
// Running (including Stopping, once Wait has filled ProcessState). Signaled deaths
// are 128+signal (see ReportExitCode).
func (p *Process) GetExitstatus() int {
	p.lock.RLock()
	defer p.lock.RUnlock()

	switch p.state {
	case Exited, Backoff, Stopped, Fatal, Stopping:
		if p.cmd == nil || p.cmd.ProcessState == nil {
			return 0
		}
		status, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus)
		if ok {
			return ReportExitCode(status)
		}
	}
	return 0
}

// GetPid returns pid of running process or 0 it is not in running status
func (p *Process) GetPid() int {
	p.lock.RLock()
	defer p.lock.RUnlock()

	if p.state == Stopped || p.state == Fatal || p.state == Unknown || p.state == Exited || p.state == Backoff {
		return 0
	}
	if p.namespaceKept.Load() {
		return int(p.keptPid.Load())
	}
	return p.cmdPid()
}

// cmdPid returns the OS pid, or 0 if the program has not been launched.
func (p *Process) cmdPid() int {
	if p.namespaceKept.Load() {
		if pid := int(p.keptPid.Load()); pid > 0 {
			return pid
		}
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// GetState returns process state
func (p *Process) GetState() State {
	return p.state
}

// GetStartTime returns process start time
func (p *Process) GetStartTime() time.Time {
	return p.startTime
}

// GetStopTime returns process stop time
func (p *Process) GetStopTime() time.Time {
	switch p.state {
	case Starting:
		fallthrough
	case Running:
		fallthrough
	case Stopping:
		return time.Unix(0, 0)
	default:
		return p.stopTime
	}
}

// GetStdoutLogfile returns program stdout log filename
func (p *Process) GetStdoutLogfile() string {
	fileName := p.config.GetStringExpression("stdout_logfile", "/dev/null")
	expandFile, err := PathExpand(fileName)
	if err != nil {
		return fileName
	}
	return expandFile
}

// GetStderrLogfile returns program stderr log filename
func (p *Process) GetStderrLogfile() string {
	fileName := p.config.GetStringExpression("stderr_logfile", "/dev/null")
	expandFile, err := PathExpand(fileName)
	if err != nil {
		return fileName
	}
	return expandFile
}

func (p *Process) getStartSeconds() int64 {
	return int64(p.config.GetInt("startsecs", 1))
}

func (p *Process) getRestartPause() int {
	return p.config.GetInt("restartpause", 0)
}

func (p *Process) getStartRetries() int32 {
	return int32(p.config.GetInt("startretries", 3))
}

func (p *Process) isAutoStart() bool {
	return p.config.GetString("autostart", "true") == "true"
}

// GetPriority returns program priority (as it set in config) with default value of 999
func (p *Process) GetPriority() int {
	return p.config.GetInt("priority", 999)
}

func (p *Process) getNumberProcs() int {
	return p.config.GetInt("numprocs", 1)
}

// SendProcessStdin sends data to process stdin
func (p *Process) SendProcessStdin(chars string) error {
	if p.stdin != nil {
		_, err := p.stdin.Write([]byte(chars))
		return err
	}
	return fmt.Errorf("NO_FILE")
}

// check if the process should be
func (p *Process) isAutoRestart() bool {
	autoRestart := p.config.GetString("autorestart", "unexpected")

	if autoRestart == "false" {
		return false
	} else if autoRestart == "true" {
		return true
	} else {
		p.lock.RLock()
		defer p.lock.RUnlock()
		if p.cmd != nil && p.cmd.ProcessState != nil {
			exitCode, err := p.getExitCode()
			// If unexpected, the process will be restarted when the program exits
			// with an exit code that is not one of the exit codes associated with
			// this process’ configuration (see exitcodes).
			return err == nil && !p.inExitCodes(exitCode)
		}
	}
	return false

}

func (p *Process) inExitCodes(exitCode int) bool {
	for _, code := range p.getExitCodes() {
		if code == exitCode {
			return true
		}
	}
	return false
}

func (p *Process) getExitCode() (int, error) {
	if p.namespaceKept.Load() {
		if code := p.keptExit.Load(); code >= 0 {
			return int(code), nil
		}
	}
	if p.cmd == nil || p.cmd.ProcessState == nil {
		return -1, fmt.Errorf("no exit code")
	}
	if status, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		return ReportExitCode(status), nil
	}

	return -1, fmt.Errorf("no exit code")

}

func (p *Process) getExitCodes() []int {
	strExitCodes := strings.Split(p.config.GetString("exitcodes", "0,2"), ",")
	result := make([]int, 0)
	for _, val := range strExitCodes {
		i, err := strconv.Atoi(val)
		if err == nil {
			result = append(result, i)
		}
	}
	return result
}

// check if the process is running or not
func (p *Process) isRunning() bool {
	if p.namespaceKept.Load() {
		return p.persistedChildAlive()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		if runtime.GOOS == "windows" {
			proc, err := ps.FindProcess(p.cmd.Process.Pid)
			return proc != nil && err == nil
		}
		return unixPidAlive(p.cmd.Process.Pid)
	}
	return false
}

// reapOrKillStrayChildBeforeRecreateCmd ensures we do not leave a live child when replacing p.cmd.
// Without this, the next start retry assigns a new *exec.Cmd while a previous goroutine may still
// be waiting on the old one only by reading p.cmd — producing multiple concurrent children (same program).
func (p *Process) reapOrKillStrayChildBeforeRecreateCmd() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if p.cmd.ProcessState != nil {
		return
	}
	pid := p.cmd.Process.Pid
	var alive bool
	if runtime.GOOS == "windows" {
		alive = p.cmd.Process.Signal(syscall.Signal(0)) == nil
	} else {
		alive = unixPidAlive(pid)
	}
	if !alive {
		return
	}
	log.WithFields(log.Fields{"program": p.GetName(), "pid": pid}).Warn(
		"terminating stray child before new exec.Cmd (fix duplicate processes when start retry replaces cmd before Wait completes)")
	old := p.cmd
	killasgroup := p.config.GetBool("killasgroup", false)
	if err := signals.Kill(old.Process, syscall.SIGKILL, killasgroup); err != nil {
		log.WithFields(log.Fields{"program": p.GetName(), "pid": pid, "err": err}).Warn("SIGKILL stray child")
	}
	done := make(chan struct{})
	go func(c *exec.Cmd) {
		_ = c.Wait()
		close(done)
	}(old)
	select {
	case <-done:
	case <-time.After(time.Duration(p.config.GetInt("stopwaitsecs", 10)) * time.Second):
		log.WithFields(log.Fields{"program": p.GetName(), "pid": pid}).Warn("timeout waiting stray child after SIGKILL")
	}
}

// create Command object for the program
func (p *Process) createProgramCommand() error {
	p.reapOrKillStrayChildBeforeRecreateCmd()

	args, err := parseCommand(p.config.GetStringExpression("command", ""))

	if err != nil {
		return err
	}
	programPath := args[0]
	cr := p.config.GetBool("container_run", false)
	usePersist := false
	if cr && !p.prepareSmolvm(&args) {
		if ok, why := p.persistNamespace(programPath); ok {
			usePersist = true
		} else if p.config.GetBool("container_run_persist", false) && why != "container_run_persist is off" && why != "container_run is off" {
			log.WithFields(log.Fields{"program": p.GetName(), "reason": why}).Info("container_run_persist ignored")
		}
		if !usePersist {
			args = createContainerRunWrapper(args)
		}
	}
	log.WithFields(log.Fields{
		"program":       p.GetName(),
		"container_run": cr,
		"orig_argv0":    programPath,
		"argv_len":      len(args),
	}).Debug("createProgramCommand: parsed command")
	p.cmd, err = createCommand(args)
	if err != nil {
		return err
	}
	// 必须先于 setUser()：当 [program] 配置了 environment= 时，setEnv 会重建 p.cmd.Env；
	// 若 setUser 在前，为 container_run 注入的 SUPERVISORD_TARGET_* 会被覆盖并丢失，导致 __run_container__
	// 最终 exec 时无法降权到 user= 指定的非 root 用户。
	p.setProgramRestartChangeMonitor(programPath)
	setDeathsig(p.cmd.SysProcAttr)
	if !p.launchedSmolvm && !usePersist {
		setContainerRun(p.cmd.SysProcAttr, p.config)
	}
	p.setEnv()
	if p.launchedSmolvm {
		// smolvm opens /dev/kvm. The guest process drops its own user.
	} else if p.setUser() != nil {
		log.WithFields(log.Fields{"user": p.config.GetString("user", "")}).Error("fail to run as user")
		return fmt.Errorf("fail to set user")
	}
	hasTargetUID := false
	hasTargetGID := false
	hasAppArmorExec := false
	for _, e := range p.cmd.Env {
		if strings.HasPrefix(e, "SUPERVISORD_TARGET_UID=") {
			hasTargetUID = true
		}
		if strings.HasPrefix(e, "SUPERVISORD_TARGET_GID=") {
			hasTargetGID = true
		}
		if strings.HasPrefix(e, "SUPERVISORD_APPARMOR_EXEC_PROFILE=") {
			hasAppArmorExec = true
		}
	}
	log.WithFields(log.Fields{
		"program":               p.GetName(),
		"defer_setuid_env_uid":  hasTargetUID,
		"defer_setuid_env_gid":  hasTargetGID,
		"has_apparmor_exec_env": hasAppArmorExec,
	}).Debug("createProgramCommand: env flags for container_run / deferred credentials")
	p.setDir()
	p.setLog()
	if usePersist {
		if err := p.ensureNamespaceKeeper(); err != nil {
			return err
		}
		p.namespaceKept.Store(true)
	} else {
		p.namespaceKept.Store(false)
	}

	if !usePersist {
		p.stdin, _ = p.cmd.StdinPipe()
	}
	return nil

}

func (p *Process) setProgramRestartChangeMonitor(programPath string) {
	if p.config.GetBool("restart_when_binary_changed", false) {
		absPath, err := filepath.Abs(programPath)
		if err != nil {
			absPath = programPath
		}
		AddProgramChangeMonitor(absPath, func(path string, mode filechangemonitor.FileChangeMode) {
			log.WithFields(log.Fields{"program": p.GetName()}).Info("program is changed, restart it")
			restart_cmd := p.config.GetString("restart_cmd_when_binary_changed", "")
			s := p.config.GetString("restart_signal_when_binary_changed", "")
			if len(restart_cmd) > 0 {
				_, err := executeCommand(restart_cmd)
				if err == nil {
					log.WithFields(log.Fields{"program": p.GetName(), "command": restart_cmd}).Info("restart program with command successfully")
				} else {
					log.WithFields(log.Fields{"program": p.GetName(), "command": restart_cmd, "error": err}).Info("fail to restart program")
				}
			} else if len(s) > 0 {
				p.sendSignals(strings.Fields(s), true)
			} else {
				p.Restart(true)
			}

		})
	}
	dirMonitor := p.config.GetString("restart_directory_monitor", "")
	filePattern := p.config.GetString("restart_file_pattern", "*")
	if dirMonitor != "" {
		absDir, err := filepath.Abs(dirMonitor)
		if err != nil {
			absDir = dirMonitor
		}
		AddConfigChangeMonitor(absDir, filePattern, func(path string, mode filechangemonitor.FileChangeMode) {
			log.WithFields(log.Fields{"program": p.GetName()}).Info("configure file for program is changed, restart it")
			restart_cmd := p.config.GetString("restart_cmd_when_file_changed", "")
			s := p.config.GetString("restart_signal_when_file_changed", "")
			if len(restart_cmd) > 0 {
				_, err := executeCommand(restart_cmd)
				if err == nil {
					log.WithFields(log.Fields{"program": p.GetName(), "command": restart_cmd}).Info("restart program with command successfully")
				} else {
					log.WithFields(log.Fields{"program": p.GetName(), "command": restart_cmd, "error": err}).Info("fail to restart program")
				}
			} else if len(s) > 0 {
				p.sendSignals(strings.Fields(s), true)
			} else {
				p.Restart(true)
			}
		})
	}

}

// wait for the started program exit
func (p *Process) waitForExit(startSecs int64) {
	if p.namespaceKept.Load() {
		p.waitPersistedChild()
		return
	}
	// Capture cmd at entry: createProgramCommand may replace p.cmd on the next start retry while this
	// goroutine runs; waiting on the wrong *exec.Cmd leaves the old child orphaned (duplicate PIDs).
	cmd := p.cmd
	if cmd == nil {
		return
	}
	_ = cmd.Wait()
	if cmd.ProcessState != nil {
		fields := log.Fields{"program": p.GetName(), "status": fmt.Sprintf("%v", cmd.ProcessState)}
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			fields["report_exit"] = ReportExitCode(ws)
			if ws.Signaled() {
				fields["signaled"] = ws.Signal().String()
			}
		}
		log.WithFields(fields).Info("program stopped")
	} else {
		log.WithFields(log.Fields{"program": p.GetName()}).Info("program stopped")
	}
	p.lock.Lock()
	defer p.lock.Unlock()
	p.stopTime = time.Now()

	// FIXME: we didn't set eventlistener logger
	// since it's stdout/stderr has been specifically managed.
	if p.StdoutLog != nil {
		p.StdoutLog.Close()
	}
	if p.StderrLog != nil {
		p.StderrLog.Close()
	}

}

// fail to start the program
func (p *Process) failToStartProgram(reason string, finishCb func()) {
	log.WithFields(log.Fields{"program": p.GetName()}).Error(reason)
	p.changeStateTo(Fatal)
	finishCb()
}

// waitForStartFileIfConfigured blocks until start_when_file exists as a non-directory path
// (regular file, fifo, socket, etc.), or deadline elapses, or stopByUser.
// Caller must not hold p.lock.
func (p *Process) waitForStartFileIfConfigured() error {
	path := strings.TrimSpace(p.config.GetString("start_when_file", ""))
	if path == "" {
		return nil
	}

	timeoutSecs := p.config.GetInt("start_when_file_timeout", 0)
	pollSecs := p.config.GetInt("start_when_file_poll_secs", 1)
	if pollSecs < 1 {
		pollSecs = 1
	}
	poll := time.Duration(pollSecs) * time.Second
	// empty/proceed：文件不存在时不阻塞，按空配置启动并打日志。默认 wait 保持原行为。
	missingMode := strings.ToLower(strings.TrimSpace(p.config.GetString("start_when_file_missing", "wait")))
	if missingMode == "empty" || missingMode == "proceed" {
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() {
			log.WithFields(log.Fields{
				"program":         p.GetName(),
				"start_when_file": path,
			}).Info("start_when_file missing, starting in empty config mode")
			return nil
		}
		log.WithFields(log.Fields{"program": p.GetName(), "start_when_file": path}).Info("start_when_file present, proceeding with launch")
		return nil
	}

	var deadline time.Time
	if timeoutSecs > 0 {
		deadline = time.Now().Add(time.Duration(timeoutSecs) * time.Second)
	}

	log.WithFields(log.Fields{
		"program":                   p.GetName(),
		"start_when_file":           path,
		"start_when_file_timeout":   timeoutSecs,
		"start_when_file_poll_secs": pollSecs,
	}).Info("waiting for start_when_file before launching command")

	for {
		p.lock.Lock()
		stopped := p.stopByUser
		p.lock.Unlock()
		if stopped {
			return errStopRequestedDuringStartFileWait
		}

		fi, err := os.Stat(path)
		if err == nil && !fi.IsDir() {
			log.WithFields(log.Fields{"program": p.GetName(), "start_when_file": path}).Info("start_when_file present, proceeding with launch")
			return nil
		}

		if timeoutSecs > 0 && time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for start_when_file %q (%ds)", path, timeoutSecs)
		}

		time.Sleep(poll)
	}
}

// monitor if the program is in running before endTime
func (p *Process) monitorProgramIsRunning(endTime time.Time, monitorExited *int32, programExited *int32) {
	// if time is not expired
	for time.Now().Before(endTime) && atomic.LoadInt32(programExited) == 0 {
		time.Sleep(time.Duration(100) * time.Millisecond)
	}
	atomic.StoreInt32(monitorExited, 1)

	p.lock.Lock()
	defer p.lock.Unlock()
	// if the program does not exit
	if atomic.LoadInt32(programExited) == 0 && p.state == Starting {
		log.WithFields(log.Fields{"program": p.GetName()}).Info("success to start program")
		p.changeStateTo(Running)
	}
}

// 这个函数可能有以下几种执行完成的情况：
//
// 1. 程序正在运行中，因此函数直接返回。
// 2. 程序尚未运行，函数开始尝试多次启动程序，直到启动成功。
// 3. 程序成功启动并正在运行中，函数启动了一个后台监视程序来监视程序运行情况，并向 `finishCb` 函数传递一个标记告知程序已停止，函数直接返回。
// 4. 程序启动失败，超出了尝试次数，函数将程序状态标记为 `FATAL`，并向 `finishCb` 函数传递一个标记告知程序已停止，函数直接返回。
// 5. 程序被终止或运行失败，超出了重试次数，函数将程序状态标记为 `EXITED`，并向 `finishCb` 函数传递一个标记告知程序已停止，函数直接返回。
func (p *Process) run(finishCb func()) {
	p.lock.Lock()
	defer p.lock.Unlock()

	// check if the program is in running state
	if p.isRunning() {
		log.WithFields(log.Fields{"program": p.GetName()}).Info("Don't start program because it is running")
		finishCb()
		return
	}

	p.startTime = time.Now()
	p.smolvmFallback = false
	p.smolvmFallbackReason = ""
	atomic.StoreInt32(p.retryTimes, 0)
	startSecs := p.getStartSeconds()
	restartPause := p.getRestartPause()
	var once sync.Once

	// finishCb can be only called one time
	finishCbWrapper := func() {
		once.Do(finishCb)
	}

	//process is not expired and not stoped by user
	for !p.stopByUser {
		if restartPause > 0 && atomic.LoadInt32(p.retryTimes) != 0 {
			// pause
			p.lock.Unlock()
			log.WithFields(log.Fields{"program": p.GetName()}).Info("don't restart the program, start it after ", restartPause, " seconds")
			time.Sleep(time.Duration(restartPause) * time.Second)
			p.lock.Lock()
		}
		p.lock.Unlock()
		if err := p.waitForStartFileIfConfigured(); err != nil {
			p.lock.Lock()
			if errors.Is(err, errStopRequestedDuringStartFileWait) {
				log.WithFields(log.Fields{"program": p.GetName()}).Info("abort start: stopped while waiting for start_when_file")
				p.changeStateTo(Stopped)
				finishCbWrapper()
				break
			}
			p.failToStartProgram(err.Error(), finishCbWrapper)
			break
		}
		p.lock.Lock()
		endTime := time.Now().Add(time.Duration(startSecs) * time.Second)
		p.changeStateTo(Starting)
		atomic.AddInt32(p.retryTimes, 1)

		err := p.createProgramCommand()
		if err != nil {
			p.failToStartProgram("fail to create program", finishCbWrapper)
			break
		}

		// Before fork: join foreground cpuset so child (init) inherits. Fixes redroid ENOSPC.
		// A persisted namespace joins once, when its keeper is created.
		if p.config.GetBool("container_run", false) && !p.namespaceKept.Load() {
			joinForegroundCpusetBeforeFork()
		}
		if p.namespaceKept.Load() {
			err = p.spawnPersistedChild()
		} else {
			err = p.cmd.Start()
		}

		if err != nil {
			if p.noteSmolvmStartFailure(err) {
				p.changeStateTo(Backoff)
				continue
			}
			if atomic.LoadInt32(p.retryTimes) >= p.getStartRetries() {
				p.failToStartProgram(fmt.Sprintf("fail to start program with error:%v", err), finishCbWrapper)
				break
			} else {
				log.WithFields(log.Fields{"program": p.GetName(), "retry": atomic.LoadInt32(p.retryTimes)}).Info("fail to start program with error:", err)
				p.changeStateTo(Backoff)
				continue
			}
		}
		childPid := p.cmdPid()
		if childPid != 0 {
			log.WithFields(log.Fields{"program": p.GetName(), "pid": childPid, "retry": atomic.LoadInt32(p.retryTimes)}).Debug("program child started")
		}
		if p.StdoutLog != nil {
			p.StdoutLog.SetPid(childPid)
		}
		if p.StderrLog != nil {
			p.StderrLog.SetPid(childPid)
		}

		// logger.CompositeLogger is not `os.File`, so `cmd.Wait()` will wait for the logger to close
		// if parent process passes its FD to child process, the logger will not close even when parent process exits
		// we need to make sure the logger is closed when the process stops running
		go func() {
			if p.namespaceKept.Load() {
				// The keeper inherits these logs for every later child. Closing them
				// on the first exit would break the next process in the same namespace.
				return
			}
			// the sleep time must be less than `stopwaitsecs`, here I set half of `stopwaitsecs`
			// otherwise the logger will not be closed before SIGKILL is sent
			halfWaitsecs := time.Duration(p.config.GetInt("stopwaitsecs", 10)/2) * time.Second
			for {
				if !p.isRunning() {
					break
				}
				time.Sleep(halfWaitsecs)
			}
			if p.StdoutLog != nil {
				p.StdoutLog.Close()
			}
			if p.StderrLog != nil {
				p.StderrLog.Close()
			}
		}()

		monitorExited := int32(0)
		programExited := int32(0)
		// Set startsec to 0 to indicate that the program needn't stay
		// running for any particular amount of time.
		if startSecs <= 0 {
			atomic.StoreInt32(&monitorExited, 1)
			log.WithFields(log.Fields{"program": p.GetName()}).Info("success to start program")
			p.changeStateTo(Running)
			go finishCbWrapper()
		} else {
			go func() {
				p.monitorProgramIsRunning(endTime, &monitorExited, &programExited)
				finishCbWrapper()
			}()
		}
		log.WithFields(log.Fields{"program": p.GetName()}).Debug("check program is starting and wait if it exit")
		p.lock.Unlock()

		procExitC := make(chan struct{})
		go func() {
			p.waitForExit(startSecs)
			close(procExitC)
		}()

		loopExit := "unknown"
	LOOP:
		for {
			select {
			case <-procExitC:
				loopExit = "child_wait_returned"
				break LOOP
			default:
				if !p.isRunning() {
					loopExit = "is_running_false"
					break LOOP
				}
			}
			time.Sleep(time.Duration(100) * time.Millisecond)
		}

		atomic.StoreInt32(&programExited, 1)
		// wait for monitor thread exit
		for atomic.LoadInt32(&monitorExited) == 0 {
			time.Sleep(time.Duration(10) * time.Millisecond)
		}

		p.lock.Lock()
		p.stopADBRelay()
		runSpec := p.containerRunSpec()
		if p.launchedSmolvm && containerPauseRequested(runSpec.stateDir, runSpec.pauseFile) {
			p.stopByUser = true
			p.launchedSmolvm = false
			p.changeStateTo(Stopped)
			log.WithFields(log.Fields{"program": p.GetName()}).Info("program paused via container_run pause file; will not restart until resume")
			break
		}
		if p.launchedSmolvm && p.state != Running && p.state != Stopping && !p.smolvmFallback {
			p.smolvmFallback = true
			p.smolvmFallbackReason = "smolvm exited before the program was ready"
			p.writeContainerRuntime(runSpec, "smolvm", "namespace", p.smolvmFallbackReason)
			p.launchedSmolvm = false
		}

		// we break the restartRetry loop if:
		// 1. process still in running after startSecs (although it's exited right now)
		// 2. it's stopping by user (we unlocked before waitForExit, so the flag stopByUser will have a chance to change).
		if p.state == Running || p.state == Stopping {
			if !p.stopByUser {
				p.changeStateTo(Exited)
				log.WithFields(log.Fields{"program": p.GetName()}).Info("program exited")
			} else {
				p.changeStateTo(Stopped)
				log.WithFields(log.Fields{"program": p.GetName()}).Info("program stopped by user")
			}
			break
		} else {
			// Child exited (or never reached Running) before monitorProgramIsRunning promoted Starting→Running.
			// This is normal supervisord behavior when the process dies within startsecs — not "duplicate programs".
			exitLog := log.Fields{
				"program":   p.GetName(),
				"retry":     atomic.LoadInt32(p.retryTimes),
				"startsecs": startSecs,
				"loop_exit": loopExit,
			}
			if p.cmd != nil && p.cmd.ProcessState != nil {
				exitLog["process_state"] = p.cmd.ProcessState.String()
				if code, err := p.getExitCode(); err == nil {
					exitLog["exit_code"] = code
				}
			}
			msg := "startup failed before startsecs (supervisord will retry until startretries); inspect child stderr / app logs (e.g. bind EADDRINUSE)"
			if loopExit == "is_running_false" {
				msg += " — loop_exit=is_running_false means Signal(0) on current p.cmd failed (child may have exited, or p.cmd no longer matches the live PID; check ps for duplicate children)"
			}
			log.WithFields(exitLog).Warn(msg)
			p.changeStateTo(Backoff)
		}

		// The number of serial failure attempts that supervisord will allow when attempting to
		// start the program before giving up and putting the process into an Fatal state
		// first start time is not the retry time
		if atomic.LoadInt32(p.retryTimes) >= p.getStartRetries() {
			p.failToStartProgram(fmt.Sprintf("fail to start program because retry times is greater than %d", p.getStartRetries()), finishCbWrapper)
			break
		}
	}

}

func (p *Process) changeStateTo(procState State) {
	if p.config.IsProgram() {
		progName := p.config.GetProgramName()
		groupName := p.config.GetGroupName()
		if procState == Starting {
			events.EmitEvent(events.CreateProcessStartingEvent(progName, groupName, p.state.String(), int(atomic.LoadInt32(p.retryTimes))))
		} else if procState == Running {
			events.EmitEvent(events.CreateProcessRunningEvent(progName, groupName, p.state.String(), p.cmdPid()))
		} else if procState == Backoff {
			events.EmitEvent(events.CreateProcessBackoffEvent(progName, groupName, p.state.String(), int(atomic.LoadInt32(p.retryTimes))))
		} else if procState == Stopping {
			events.EmitEvent(events.CreateProcessStoppingEvent(progName, groupName, p.state.String(), p.cmdPid()))
		} else if procState == Exited {
			exitCode, err := p.getExitCode()
			expected := 0
			hasExit := err == nil
			if hasExit && p.inExitCodes(exitCode) {
				expected = 1
			}
			fields := log.Fields{
				"program":  progName,
				"from":     p.state.String(),
				"expected": expected == 1,
			}
			exitForEvent := 0
			if hasExit {
				fields["report_exit"] = exitCode
				exitForEvent = exitCode
				if p.cmd != nil && p.cmd.ProcessState != nil {
					if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
						fields["detail"] = FormatExitDescription(ws)
					}
				}
			}
			log.WithFields(fields).Info("program state -> Exited")
			events.EmitEvent(events.CreateProcessExitedEvent(progName, groupName, p.state.String(), expected, p.cmdPid(), exitForEvent, hasExit))
		} else if procState == Fatal {
			log.WithFields(log.Fields{"program": progName, "from": p.state.String()}).Error("program state -> Fatal")
			events.EmitEvent(events.CreateProcessFatalEvent(progName, groupName, p.state.String()))
		} else if procState == Stopped {
			// cmd/Process is nil when stop arrives before launch (start_when_file wait).
			fields := log.Fields{"program": progName, "from": p.state.String(), "stopped_by_user": p.stopByUser}
			exitCode, exitErr := p.getExitCode()
			hasExit := exitErr == nil
			if hasExit {
				fields["report_exit"] = exitCode
				if p.cmd != nil && p.cmd.ProcessState != nil {
					if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
						fields["detail"] = FormatExitDescription(ws)
					}
				}
			}
			log.WithFields(fields).Info("program state -> Stopped")
			events.EmitEvent(events.CreateProcessStoppedEvent(progName, groupName, p.state.String(), p.cmdPid(), exitCode, hasExit))
		} else if procState == Unknown {
			events.EmitEvent(events.CreateProcessUnknownEvent(progName, groupName, p.state.String()))
		}
	}
	p.state = procState
}

// Signal sends signal to the process
//
// Args:
//
//	sig - the signal to the process
//	sigChildren - if true, sends the same signal to the process and its children
func (p *Process) Signal(sig os.Signal, sigChildren bool) error {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.sendSignal(sig, sigChildren)
}

func (p *Process) sendSignals(sigs []string, sigChildren bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for _, strSig := range sigs {
		sig, err := signals.ToSignal(strSig)
		if err == nil {
			p.sendSignal(sig, sigChildren)
		} else {
			log.WithFields(log.Fields{"program": p.GetName(), "signal": strSig}).Info("Invalid signal name")
		}
	}
}

// send signal to the process
//
// Args:
//
//	sig - the signal to be sent
//	sigChildren - if true, the signal also will be sent to children processes too
func (p *Process) sendSignal(sig os.Signal, sigChildren bool) error {
	if p.namespaceKept.Load() && p.keeper != nil {
		return p.signalPersistedChild(sig)
	}
	if p.cmd != nil && p.cmd.Process != nil {
		log.WithFields(log.Fields{"program": p.GetName(), "signal": sig}).Info("Send signal to program")
		err := signals.Kill(p.cmd.Process, sig, sigChildren)
		return err
	}
	return fmt.Errorf("process is not started")
}

func (p *Process) setEnv() {
	envFromFiles := p.config.GetEnvFromFiles("envFiles")
	env := p.config.GetEnv("environment")
	envOverlay := p.config.GetEnvFromFiles("envFilesOverlay")

	var base []string
	if len(env)+len(envFromFiles) != 0 {
		// envFiles + [program]environment 必须先于 os.Environ 参与 merge：旧顺序把 os 放在前，
		// 导致 program 中的 HOME= 等无法覆盖 supervisord（root）继承的环境；user= 降权后的
		// 子进程仍可能带着 HOME=/root，业务读错配置路径后秒退 → supervisord 报 Fatal。
		base = mergeKeyValueArrays(append(envFromFiles, env...), os.Environ())
	} else {
		base = mergeKeyValueArrays(p.cmd.Env, os.Environ())
	}
	// envFilesOverlay / runtime .env：在 environment= 与 envFiles 之后合并，便于 API 持久化覆盖同名字段。
	if len(envOverlay) > 0 {
		p.cmd.Env = mergeKeyValueArrays(envOverlay, base)
	} else {
		p.cmd.Env = base
	}
	// Persist resolv.conf in supervisord namespace to shared path.
	// The container-run helper (init namespace) reads /shared/etc/resolv.conf and writes to /system/etc/resolv.conf.
	if p.config.GetBool("container_run", false) {
		maybeInjectContainerRunAppArmorEnv(p)
		if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
			_ = os.MkdirAll("/shared/etc", 0755)
			_ = os.WriteFile("/shared/etc/resolv.conf", data, 0644)
		}
	}
}

// 辅助函数：带覆盖的环境变量追加
func mergeKeyValueArrays(arr1, arr2 []string) []string {
	keySet := make(map[string]bool)
	result := make([]string, 0, len(arr1)+len(arr2))

	// 处理第一个数组，保留所有元素
	for _, item := range arr1 {
		if key := strings.SplitN(item, "=", 2)[0]; key != "" {
			keySet[key] = true
		}
		result = append(result, item)
	}

	// 处理第二个数组，跳过已存在的键
	for _, item := range arr2 {
		if key := strings.SplitN(item, "=", 2)[0]; key != "" {
			if !keySet[key] {
				result = append(result, item)
			}
		}
	}

	return result
}

func (p *Process) setDir() {
	dir := p.config.GetStringExpression("directory", "")
	if dir != "" {
		p.cmd.Dir = dir
	}
}

func (p *Process) setLog() {
	if p.config.IsProgram() {
		p.StdoutLog = p.createStdoutLogger()
		captureBytes := p.config.GetBytes("stdout_capture_maxbytes", 0)
		if captureBytes > 0 {
			log.WithFields(log.Fields{"program": p.config.GetProgramName()}).Info("capture stdout process communication")
			p.StdoutLog = logger.NewLogCaptureLogger(p.StdoutLog,
				captureBytes,
				"PROCESS_COMMUNICATION_STDOUT",
				p.GetName(),
				p.GetGroup())
		}

		p.cmd.Stdout = p.StdoutLog

		if p.config.GetBool("redirect_stderr", false) {
			p.StderrLog = p.StdoutLog
		} else {
			p.StderrLog = p.createStderrLogger()
		}

		captureBytes = p.config.GetBytes("stderr_capture_maxbytes", 0)

		if captureBytes > 0 {
			log.WithFields(log.Fields{"program": p.config.GetProgramName()}).Info("capture stderr process communication")
			p.StderrLog = logger.NewLogCaptureLogger(p.StdoutLog,
				captureBytes,
				"PROCESS_COMMUNICATION_STDERR",
				p.GetName(),
				p.GetGroup())
		}

		p.cmd.Stderr = p.StderrLog

	} else if p.config.IsEventListener() {
		in, err := p.cmd.StdoutPipe()
		if err != nil {
			log.WithFields(log.Fields{"eventListener": p.config.GetEventListenerName()}).Error("fail to get stdin")
			return
		}
		out, err := p.cmd.StdinPipe()
		if err != nil {
			log.WithFields(log.Fields{"eventListener": p.config.GetEventListenerName()}).Error("fail to get stdout")
			return
		}
		events := strings.Split(p.config.GetString("events", ""), ",")
		for i, event := range events {
			events[i] = strings.TrimSpace(event)
		}
		p.cmd.Stderr = os.Stderr

		p.registerEventListener(p.config.GetEventListenerName(),
			events,
			in,
			out)
	}
}

func (p *Process) createStdoutLogEventEmitter() logger.LogEventEmitter {
	if p.config.GetBytes("stdout_capture_maxbytes", 0) <= 0 && p.config.GetBool("stdout_events_enabled", false) {
		return logger.NewStdoutLogEventEmitter(p.config.GetProgramName(), p.config.GetGroupName(), func() int {
			return p.GetPid()
		})
	}
	return logger.NewNullLogEventEmitter()
}

func (p *Process) createStderrLogEventEmitter() logger.LogEventEmitter {
	if p.config.GetBytes("stderr_capture_maxbytes", 0) <= 0 && p.config.GetBool("stderr_events_enabled", false) {
		return logger.NewStdoutLogEventEmitter(p.config.GetProgramName(), p.config.GetGroupName(), func() int {
			return p.GetPid()
		})
	}
	return logger.NewNullLogEventEmitter()
}

func (p *Process) registerEventListener(eventListenerName string,
	_events []string,
	stdin io.Reader,
	stdout io.Writer) {
	eventListener := events.NewEventListener(eventListenerName,
		p.supervisorID,
		stdin,
		stdout,
		p.config.GetInt("buffer_size", 100))
	events.RegisterEventListener(eventListenerName, _events, eventListener)
}

func (p *Process) unregisterEventListener(eventListenerName string) {
	events.UnregisterEventListener(eventListenerName)
}

func (p *Process) createStdoutLogger() logger.Logger {
	logFile := p.GetStdoutLogfile()
	maxBytes := int64(p.config.GetBytes("stdout_logfile_maxbytes", 50*1024*1024))
	backups := p.config.GetInt("stdout_logfile_backups", 10)
	logEventEmitter := p.createStdoutLogEventEmitter()
	props := make(map[string]string)
	syslog_facility := p.config.GetString("syslog_facility", "")
	syslog_tag := p.config.GetString("syslog_tag", "")
	syslog_priority := p.config.GetString("syslog_stdout_priority", "")

	if len(syslog_facility) > 0 {
		props["syslog_facility"] = syslog_facility
	}
	if len(syslog_tag) > 0 {
		props["syslog_tag"] = syslog_tag
	}
	if len(syslog_priority) > 0 {
		props["syslog_priority"] = syslog_priority
	}

	return logger.NewLogger(p.GetName(), logFile, logger.NewNullLocker(), maxBytes, backups, props, logEventEmitter)
}

func (p *Process) createStderrLogger() logger.Logger {
	logFile := p.GetStderrLogfile()
	maxBytes := int64(p.config.GetBytes("stderr_logfile_maxbytes", 50*1024*1024))
	backups := p.config.GetInt("stderr_logfile_backups", 10)
	logEventEmitter := p.createStderrLogEventEmitter()
	props := make(map[string]string)
	syslog_facility := p.config.GetString("syslog_facility", "")
	syslog_tag := p.config.GetString("syslog_tag", "")
	syslog_priority := p.config.GetString("syslog_stderr_priority", "")

	if len(syslog_facility) > 0 {
		props["syslog_facility"] = syslog_facility
	}
	if len(syslog_tag) > 0 {
		props["syslog_tag"] = syslog_tag
	}
	if len(syslog_priority) > 0 {
		props["syslog_priority"] = syslog_priority
	}

	return logger.NewLogger(p.GetName(), logFile, logger.NewNullLocker(), maxBytes, backups, props, logEventEmitter)
}

// envSliceLookup returns the raw value for key in a KEY=VAL environment slice (os.Environ format).
func envSliceLookup(env []string, key string) (val string, ok bool) {
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 0 || parts[0] != key {
			continue
		}
		if len(parts) == 2 {
			return parts[1], true
		}
		return "", true // "KEY" or "KEY=" with no value
	}
	return "", false
}

func (p *Process) setUser() error {
	userName := p.config.GetString("user", "")
	if len(userName) == 0 {
		return nil
	}

	// check if group is provided
	pos := strings.Index(userName, ":")
	groupName := ""
	if pos != -1 {
		groupName = userName[pos+1:]
		userName = userName[0:pos]
	}
	u, err := user.Lookup(userName)
	if err != nil {
		return err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil && groupName == "" {
		return err
	}
	if groupName != "" {
		g, err := user.LookupGroup(groupName)
		if err != nil {
			return err
		}
		gid, err = strconv.ParseUint(g.Gid, 10, 32)
		if err != nil {
			return err
		}
	}
	if p.config.GetBool("container_run", false) {
		// container_run 需要 wrapper(__run_container__) 先以 root 完成 mount/proc 等初始化，
		// 再在最终 exec 前按 program user 降权到目标 uid/gid。
		p.cmd.Env = appendEnvWithOverride(p.cmd.Env,
			"SUPERVISORD_TARGET_UID", strconv.FormatUint(uid, 10),
			"SUPERVISORD_TARGET_GID", strconv.FormatUint(gid, 10),
		)
		log.WithFields(log.Fields{
			"program": p.GetName(),
			"user":    userName,
			"uid":     uid,
			"gid":     gid,
		}).Info("container_run enabled: defer user switch to __run_container__ final exec")
	} else {
		setUserID(p.cmd.SysProcAttr, uint32(uid), uint32(gid))
	}

	// 强制设置关键环境变量：HOME/USER/LOGNAME 始终按目标用户覆盖。
	// PATH：若 setEnv 合并后已有非空 PATH（来自 [program] environment=、envFilesOverlay 或镜像继承），则保留；
	// 否则使用 defaultPath，避免降权后继承 root 的异常 PATH，同时保证有基本系统路径。
	p.cmd.Env = appendEnvWithOverride(p.cmd.Env,
		"HOME", u.HomeDir, // 强制HOME目录
		"USER", u.Username, // 用户名
		"LOGNAME", u.Username, // 登录名
	)
	pathVal, pathOK := envSliceLookup(p.cmd.Env, "PATH")
	if !pathOK || strings.TrimSpace(pathVal) == "" {
		p.cmd.Env = appendEnvWithOverride(p.cmd.Env,
			"PATH", defaultPath(u), // 安全PATH
		)
	}

	// 删除root残留的环境变量
	filterRootEnv(&p.cmd.Env)

	return nil
}

// 辅助函数：带覆盖的环境变量追加
func appendEnvWithOverride(env []string, pairs ...string) []string {
	newEnv := make([]string, 0, len(env)+len(pairs)/2)
	set := make(map[string]bool)

	// 先添加新变量
	for i := 0; i < len(pairs); i += 2 {
		key := pairs[i]
		value := pairs[i+1]
		newEnv = append(newEnv, fmt.Sprintf("%s=%s", key, value))
		set[key] = true
	}

	// 保留未覆盖的旧变量
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) < 2 || set[parts[0]] {
			continue
		}
		newEnv = append(newEnv, e)
	}

	return newEnv
}

// 辅助函数：生成安全PATH
func defaultPath(u *user.User) string {
	// 根据用户类型返回不同PATH
	if u.Uid == "0" {
		return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	return "/usr/local/bin:/usr/bin:/bin:/usr/local/games:/usr/games"
}

// 辅助函数：过滤危险的环境变量
func filterRootEnv(env *[]string) {
	filtered := make([]string, 0, len(*env))
	for _, e := range *env {
		if strings.HasPrefix(e, "SUDO_") ||
			strings.HasPrefix(e, "XDG_RUNTIME_DIR=") {
			continue
		}
		filtered = append(filtered, e)
	}
	*env = filtered
}

// Stop sends signal to process to make it quit.
// If immediate is true, sends SIGKILL immediately (same effect as kill -9 for the supervised process tree when killasgroup applies).
// If wait is false, returns once the kill path has been launched without waiting for the process to exit.
// If wait is true, blocks until the supervised child is no longer running (or a hard deadline elapses).
func (p *Process) Stop(wait bool, immediate bool) {
	p.lock.Lock()
	p.stopByUser = true
	isRunning := p.isRunning()
	p.lock.Unlock()
	if !isRunning {
		log.WithFields(log.Fields{"program": p.GetName()}).Info("program is not running")
		return
	}

	log.WithFields(log.Fields{"program": p.GetName()}).Info("stopping the program")
	p.changeStateTo(Stopping)
	stopasgroup := p.config.GetBool("stopasgroup", false)
	killasgroup := p.config.GetBool("killasgroup", stopasgroup)
	if stopasgroup && !killasgroup {
		log.WithFields(log.Fields{"program": p.GetName()}).Error("Cannot set stopasgroup=true and killasgroup=false")
	}

	sigs := strings.Fields(p.config.GetString("stopsignal", "TERM"))
	waitsecs := time.Duration(p.config.GetInt("stopwaitsecs", 10)) * time.Second
	killwaitsecs := time.Duration(p.config.GetInt("killwaitsecs", 2)) * time.Second
	// Hard ceiling so Wait=true callers (ctl stop) do not return while the child is still alive.
	sigRounds := len(sigs)
	if sigRounds == 0 {
		sigRounds = 1
	}
	exitDeadline := time.Now().Add(waitsecs*time.Duration(sigRounds) + killwaitsecs + 5*time.Second)
	if immediate {
		exitDeadline = time.Now().Add(killwaitsecs + 5*time.Second)
	}

	var stopped int32 = 0
	if immediate {
		p.lock.Lock()
		p.lastImmediateStop = time.Now()
		p.lock.Unlock()
		go func() {
			log.WithFields(log.Fields{"program": p.GetName()}).Info("immediate stop: SIGKILL")
			if err := p.Signal(syscall.SIGKILL, killasgroup); err != nil {
				log.WithFields(log.Fields{"program": p.GetName(), "err": err}).Warn(
					"immediate SIGKILL failed (container may lack CAP_KILL; configure stop_http_url for cooperative shutdown)")
			}
			atomic.StoreInt32(&stopped, 1)
		}()
	} else {
		go func() {
			for i := 0; i < len(sigs) && atomic.LoadInt32(&stopped) == 0; i++ {
				// send signal to process
				sig, err := signals.ToSignal(sigs[i])
				if err != nil {
					continue
				}
				log.WithFields(log.Fields{"program": p.GetName(), "signal": sigs[i]}).Info("send stop signal to program")
				p.Signal(sig, stopasgroup)
				endTime := time.Now().Add(waitsecs)
				// wait at most "stopwaitsecs" seconds for one signal
				for endTime.After(time.Now()) {
					// if it already exits
					if p.state != Starting && p.state != Running && p.state != Stopping {
						atomic.StoreInt32(&stopped, 1)
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if atomic.LoadInt32(&stopped) == 0 {
				log.WithFields(log.Fields{"program": p.GetName()}).Info("force to kill the program (SIGKILL; report_exit expect 137 if Wait sees signaled)")
				p.Signal(syscall.SIGKILL, killasgroup)
				killEndTime := time.Now().Add(killwaitsecs)
				for killEndTime.After(time.Now()) {
					// if it exits
					if p.state != Starting && p.state != Running && p.state != Stopping {
						atomic.StoreInt32(&stopped, 1)
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				atomic.StoreInt32(&stopped, 1)
			}
		}()
	}
	if !wait {
		return
	}
	for atomic.LoadInt32(&stopped) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	for p.isRunning() && time.Now().Before(exitDeadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if p.isRunning() {
		log.WithFields(log.Fields{"program": p.GetName()}).Warn("stop wait deadline elapsed; supervised child still running")
	}
}

// GetStatus returns a short process wait summary. Signaled deaths are not
// reported as "running" (unlike ProcessState.Exited()-only checks).
func (p *Process) GetStatus() string {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if p.cmd == nil || p.cmd.ProcessState == nil {
		return "<nil>"
	}
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && WaitFinished(ws) {
		return FormatExitDescription(ws)
	}
	if p.cmd.ProcessState.Exited() {
		return p.cmd.ProcessState.String()
	}
	return "running"
}
