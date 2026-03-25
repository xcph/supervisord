package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/jessevdk/go-flags"
	"github.com/ochinchina/supervisord/config"
	"github.com/ochinchina/supervisord/types"
	"github.com/ochinchina/supervisord/xmlrpcclient"
)

// CtlCommand the entry of ctl command
type CtlCommand struct {
	ServerURL string `short:"s" long:"serverurl" description:"URL on which supervisord server is listening"`
	User      string `short:"u" long:"user" description:"the user name"`
	Password  string `short:"P" long:"password" description:"the password"`
	Verbose   bool   `short:"v" long:"verbose" description:"Show verbose debug information"`
}

// StatusCommand get the status of all supervisor managed programs
type StatusCommand struct {
}

// StartCommand start the given program
type StartCommand struct {
}

// StopCommand stop the given program
type StopCommand struct {
}

// RestartCommand restart the given program
type RestartCommand struct {
}

// ShutdownCommand shutdown the supervisor
type ShutdownCommand struct {
}

// ReloadCommand reload all the programs
type ReloadCommand struct {
}

// PidCommand get the pid of program
type PidCommand struct {
}

// SignalCommand send signal of program
type SignalCommand struct {
}

// LogtailCommand tail the stdout/stderr log of program through http interface
type LogtailCommand struct {
}

// ExecCommand enter program's namespace and run shell or command (kubectl-exec-like: -i -t, optional -- before command).
type ExecCommand struct {
	Stdin bool `short:"i" long:"stdin" description:"Pass stdin to the remote command"`
	Tty   bool `short:"t" long:"tty" description:"Allocate a pseudo-TTY (interactive shell); implies -i"`
}

// CmdCheckWrapperCommand A wrapper can be used to check whether
// number of parameters is valid or not
type CmdCheckWrapperCommand struct {
	// Original cmd
	cmd flags.Commander
	// leastNumArgs indicates how many arguments
	// this cmd should have at least
	leastNumArgs int
	// Print usage when arguments not valid
	usage string
}

// execCheckWrapper embeds ExecCommand so go-flags registers -i/-t on the ctl exec subcommand.
// CmdCheckWrapperCommand{cmd: &ExecCommand{}} hides ExecCommand fields from reflection (lowercase cmd),
// which caused "supervisord ctl exec -it ..." to fail with unknown flag `i'.
type execCheckWrapper struct {
	ExecCommand
}

const execCmdUsage = "exec [options] <program> [--] [command...]"

func (e *execCheckWrapper) Execute(args []string) error {
	if len(args) < 1 {
		err := fmt.Errorf("Invalid arguments.\nUsage: supervisord ctl %s", execCmdUsage)
		fmt.Printf("%v\n", err)
		return err
	}
	return e.ExecCommand.Execute(args)
}

var ctlCommand CtlCommand
var statusCommand = CmdCheckWrapperCommand{&StatusCommand{}, 0, ""}
var startCommand = CmdCheckWrapperCommand{&StartCommand{}, 0, ""}
var stopCommand = CmdCheckWrapperCommand{&StopCommand{}, 0, ""}
var restartCommand = CmdCheckWrapperCommand{&RestartCommand{}, 0, ""}
var shutdownCommand = CmdCheckWrapperCommand{&ShutdownCommand{}, 0, ""}
var reloadCommand = CmdCheckWrapperCommand{&ReloadCommand{}, 0, ""}
var pidCommand = CmdCheckWrapperCommand{&PidCommand{}, 1, "pid <program>"}
var signalCommand = CmdCheckWrapperCommand{&SignalCommand{}, 2, "signal <signal_name> <program>[...]"}
var logtailCommand = CmdCheckWrapperCommand{&LogtailCommand{}, 1, "logtail <program>"}
var execWrapper = &execCheckWrapper{}

func (x *CtlCommand) getServerURL() string {
	options.Configuration, _ = findSupervisordConf()

	if x.ServerURL != "" {
		return x.ServerURL
	} else if _, err := os.Stat(options.Configuration); err == nil {
		myconfig := config.NewConfig(options.Configuration)
		myconfig.Load()
		if entry, ok := myconfig.GetSupervisorctl(); ok {
			serverurl := entry.GetString("serverurl", "")
			if serverurl != "" {
				return serverurl
			}
		}
	}
	return "http://localhost:9001"
}

func (x *CtlCommand) getUser() string {
	options.Configuration, _ = findSupervisordConf()

	if x.User != "" {
		return x.User
	} else if _, err := os.Stat(options.Configuration); err == nil {
		myconfig := config.NewConfig(options.Configuration)
		myconfig.Load()
		if entry, ok := myconfig.GetSupervisorctl(); ok {
			user := entry.GetString("username", "")
			return user
		}
	}
	return ""
}

func (x *CtlCommand) getPassword() string {
	options.Configuration, _ = findSupervisordConf()

	if x.Password != "" {
		return x.Password
	} else if _, err := os.Stat(options.Configuration); err == nil {
		myconfig := config.NewConfig(options.Configuration)
		myconfig.Load()
		if entry, ok := myconfig.GetSupervisorctl(); ok {
			password := entry.GetString("password", "")
			return password
		}
	}
	return ""
}

func (x *CtlCommand) createRPCClient() *xmlrpcclient.XMLRPCClient {
	rpcc := xmlrpcclient.NewXMLRPCClient(x.getServerURL(), x.Verbose)
	rpcc.SetUser(x.getUser())
	rpcc.SetPassword(x.getPassword())
	return rpcc
}

// Execute implements flags.Commander interface to execute the control commands
func (x *CtlCommand) Execute(args []string) error {
	if len(args) == 0 {
		return nil
	}

	rpcc := x.createRPCClient()
	verb := args[0]

	switch verb {

	////////////////////////////////////////////////////////////////////////////////
	// STATUS
	////////////////////////////////////////////////////////////////////////////////
	case "status":
		x.status(rpcc, args[1:])

		////////////////////////////////////////////////////////////////////////////////
		// START or STOP
		////////////////////////////////////////////////////////////////////////////////
	case "start", "stop":
		x.startStopProcesses(rpcc, verb, args[1:])

		////////////////////////////////////////////////////////////////////////////////
		// SHUTDOWN
		////////////////////////////////////////////////////////////////////////////////
	case "shutdown":
		x.shutdown(rpcc)
	case "reload":
		x.reload(rpcc)
	case "signal":
		sigName, processes := args[1], args[2:]
		x.signal(rpcc, sigName, processes)
	case "pid":
		x.getPid(rpcc, args[1])
	default:
		fmt.Println("unknown command")
	}

	return nil
}

// get the status of processes
func (x *CtlCommand) status(rpcc *xmlrpcclient.XMLRPCClient, processes []string) {
	processesMap := make(map[string]bool)
	for _, process := range processes {
		processesMap[process] = true
	}
	if reply, err := rpcc.GetAllProcessInfo(); err == nil {
		x.showProcessInfo(&reply, processesMap)
	} else {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}

// start or stop the processes
// verb must be: start or stop
func (x *CtlCommand) startStopProcesses(rpcc *xmlrpcclient.XMLRPCClient, verb string, processes []string) {
	state := map[string]string{
		"start": "started",
		"stop":  "stopped",
	}
	x._startStopProcesses(rpcc, verb, processes, state[verb], true)
}

func (x *CtlCommand) _startStopProcesses(rpcc *xmlrpcclient.XMLRPCClient, verb string, processes []string, state string, showProcessInfo bool) {
	if len(processes) <= 0 {
		fmt.Printf("Please specify process for %s\n", verb)
	}
	for _, pname := range processes {
		if pname == "all" {
			reply, err := rpcc.ChangeAllProcessState(verb)
			if err == nil {
				if showProcessInfo {
					x.showProcessInfo(&reply, make(map[string]bool))
				}
			} else {
				fmt.Printf("Fail to change all process state to %s", state)
			}
		} else {
			if reply, err := rpcc.ChangeProcessState(verb, pname); err == nil {
				if showProcessInfo {
					fmt.Printf("%s: ", pname)
					if !reply.Value {
						fmt.Printf("not ")
					}
					fmt.Printf("%s\n", state)
				}
			} else {
				fmt.Printf("%s: failed [%v]\n", pname, err)
				os.Exit(1)
			}
		}
	}
}

func (x *CtlCommand) restartProcesses(rpcc *xmlrpcclient.XMLRPCClient, processes []string) {
	x._startStopProcesses(rpcc, "stop", processes, "stopped", false)
	x._startStopProcesses(rpcc, "start", processes, "restarted", true)
}

// shutdown the supervisord
func (x *CtlCommand) shutdown(rpcc *xmlrpcclient.XMLRPCClient) {
	if reply, err := rpcc.Shutdown(); err == nil {
		if reply.Value {
			fmt.Printf("Shut Down\n")
		} else {
			fmt.Printf("Hmmm! Something gone wrong?!\n")
		}
	} else {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}

// reload all the programs in the supervisord
func (x *CtlCommand) reload(rpcc *xmlrpcclient.XMLRPCClient) {
	if reply, err := rpcc.ReloadConfig(); err == nil {

		if len(reply.AddedGroup) > 0 {
			fmt.Printf("Added Groups: %s\n", strings.Join(reply.AddedGroup, ","))
		}
		if len(reply.ChangedGroup) > 0 {
			fmt.Printf("Changed Groups: %s\n", strings.Join(reply.ChangedGroup, ","))
		}
		if len(reply.RemovedGroup) > 0 {
			fmt.Printf("Removed Groups: %s\n", strings.Join(reply.RemovedGroup, ","))
		}
	} else {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}

// send signal to one or more processes
func (x *CtlCommand) signal(rpcc *xmlrpcclient.XMLRPCClient, sigName string, processes []string) {
	for _, process := range processes {
		if process == "all" {
			reply, err := rpcc.SignalAll(process)
			if err == nil {
				x.showProcessInfo(&reply, make(map[string]bool))
			} else {
				fmt.Printf("Fail to send signal %s to all process", sigName)
				os.Exit(1)
			}
		} else {
			reply, err := rpcc.SignalProcess(sigName, process)
			if err == nil && reply.Success {
				fmt.Printf("Succeed to send signal %s to process %s\n", sigName, process)
			} else {
				fmt.Printf("Fail to send signal %s to process %s\n", sigName, process)
				os.Exit(1)
			}
		}
	}
}

// get the pid of running program
func (x *CtlCommand) getPid(rpcc *xmlrpcclient.XMLRPCClient, process string) {
	procInfo, err := rpcc.GetProcessInfo(process)
	if err != nil {
		fmt.Printf("program '%s' not found\n", process)
		os.Exit(1)
	} else {
		fmt.Printf("%d\n", procInfo.Pid)
	}
}

// splitExecArgs splits program name and command after optional "--" (same idea as kubectl exec).
func isCtlInteractiveShellArgv(arg string) bool {
	switch filepath.Base(arg) {
	case "sh", "bash", "ash", "ksh":
		return true
	default:
		return false
	}
}

func splitExecArgs(args []string) (program string, cmdArgs []string) {
	if len(args) == 0 {
		return "", nil
	}
	for i, a := range args {
		if a == "--" {
			if i == 0 {
				fmt.Fprintf(os.Stderr, "exec: program name must appear before --\n")
				os.Exit(2)
			}
			return args[0], args[i+1:]
		}
	}
	return args[0], args[1:]
}

// exec enters the program's PID and mount namespace, then runs a command or interactive shell.
// Usage: exec [-i] [-t] <program> [--] [command...]
// With -t and no command, runs $SHELL or /bin/bash. Without -t, a command is required (e.g. exec init -- ls -la).
func (x *CtlCommand) exec(rpcc *xmlrpcclient.XMLRPCClient, args []string, stdinFlag, ttyFlag bool) {
	program, cmdArgs := splitExecArgs(args)
	if program == "" {
		fmt.Fprintf(os.Stderr, "exec: program name is required\n")
		os.Exit(2)
	}
	procInfo, err := rpcc.GetProcessInfo(program)
	if err != nil {
		fmt.Fprintf(os.Stderr, "program '%s' not found\n", program)
		os.Exit(1)
	}
	if procInfo.Pid == 0 {
		fmt.Fprintf(os.Stderr, "program '%s' is not running (pid=0)\n", program)
		os.Exit(1)
	}

	useTTY := ttyFlag
	useStdin := stdinFlag
	if useTTY && !useStdin {
		useStdin = true // same as kubectl: -t implies attaching stdin
	}
	// Without -i, Go exec gives the child /dev/null for stdin. A lone `sh`/`bash` looks interactive
	// but cannot read the keyboard — use stdin from the terminal (same idea as `docker run -it`).
	if !useStdin && len(cmdArgs) == 1 && isCtlInteractiveShellArgv(cmdArgs[0]) {
		useStdin = true
	}

	if len(cmdArgs) == 0 {
		if useTTY {
			shell := os.Getenv("SHELL")
			if shell == "" {
				shell = "/bin/bash"
			}
			cmdArgs = []string{shell}
		} else {
			fmt.Fprintf(os.Stderr, "exec: use -t for an interactive shell, or pass a command (e.g. exec %s -- ls)\n", program)
			os.Exit(2)
		}
	}

	var stdin *os.File
	if useStdin {
		stdin = os.Stdin
	}
	if err := runExecInNamespace(procInfo.Pid, cmdArgs, stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "exec failed: %v\n", err)
		os.Exit(1)
	}
}

func (x *CtlCommand) getProcessInfo(rpcc *xmlrpcclient.XMLRPCClient, process string) (types.ProcessInfo, error) {
	return rpcc.GetProcessInfo(process)
}

// check if group name should be displayed
func (x *CtlCommand) showGroupName() bool {
	val, ok := os.LookupEnv("SUPERVISOR_GROUP_DISPLAY")
	if !ok {
		return false
	}

	val = strings.ToLower(val)
	return val == "yes" || val == "true" || val == "y" || val == "t" || val == "1"
}

func (x *CtlCommand) showProcessInfo(reply *xmlrpcclient.AllProcessInfoReply, processesMap map[string]bool) {
	for _, pinfo := range reply.Value {
		description := pinfo.Description
		if strings.ToLower(description) == "<string></string>" {
			description = ""
		}
		if x.inProcessMap(&pinfo, processesMap) {
			processName := pinfo.GetFullName()
			if !x.showGroupName() {
				processName = pinfo.Name
			}
			fmt.Printf("%s%-33s%-10s%s%s\n", x.getANSIColor(strings.ToUpper(pinfo.Statename)), processName, pinfo.Statename, description, "\x1b[0m")
		}
	}
}

func (x *CtlCommand) inProcessMap(procInfo *types.ProcessInfo, processesMap map[string]bool) bool {
	if len(processesMap) <= 0 {
		return true
	}
	for procName := range processesMap {
		if procName == procInfo.Name || procName == procInfo.GetFullName() {
			return true
		}

		// check the wildcast '*'
		pos := strings.Index(procName, ":")
		if pos != -1 {
			groupName := procName[0:pos]
			programName := procName[pos+1:]
			if programName == "*" && groupName == procInfo.Group {
				return true
			}
		}
	}
	return false
}

func (x *CtlCommand) getANSIColor(statename string) string {
	if statename == "RUNNING" {
		// green
		return "\x1b[0;32m"
	} else if statename == "BACKOFF" || statename == "FATAL" {
		// red
		return "\x1b[0;31m"
	} else {
		// yellow
		return "\x1b[1;33m"
	}
}

// Execute implements flags.Commander interface to get status of program
func (sc *StatusCommand) Execute(args []string) error {
	ctlCommand.status(ctlCommand.createRPCClient(), args)
	return nil
}

// Execute start the given programs
func (sc *StartCommand) Execute(args []string) error {
	ctlCommand.startStopProcesses(ctlCommand.createRPCClient(), "start", args)
	return nil
}

// Execute stop the given programs
func (sc *StopCommand) Execute(args []string) error {
	ctlCommand.startStopProcesses(ctlCommand.createRPCClient(), "stop", args)
	return nil
}

// Execute restart the programs
func (rc *RestartCommand) Execute(args []string) error {
	ctlCommand.restartProcesses(ctlCommand.createRPCClient(), args)
	return nil
}

// Execute shutdown the supervisor
func (sc *ShutdownCommand) Execute(args []string) error {
	ctlCommand.shutdown(ctlCommand.createRPCClient())
	return nil
}

// Execute stop the running programs and reload the supervisor configuration
func (rc *ReloadCommand) Execute(args []string) error {
	ctlCommand.reload(ctlCommand.createRPCClient())
	return nil
}

// Execute send signal to program
func (rc *SignalCommand) Execute(args []string) error {
	sigName, processes := args[0], args[1:]
	ctlCommand.signal(ctlCommand.createRPCClient(), sigName, processes)
	return nil
}

// Execute get the pid of program
func (pc *PidCommand) Execute(args []string) error {
	ctlCommand.getPid(ctlCommand.createRPCClient(), args[0])
	return nil
}

// Execute enter program's namespace and run shell or command
func (ec *ExecCommand) Execute(args []string) error {
	ctlCommand.exec(ctlCommand.createRPCClient(), args, ec.Stdin, ec.Tty)
	return nil
}

// Execute tail the stdout/stderr of a program through http interface
func (lc *LogtailCommand) Execute(args []string) error {
	program := args[0]
	go func() {
		lc.tailLog(program, "stderr")
	}()
	return lc.tailLog(program, "stdout")
}

func (lc *LogtailCommand) tailLog(program string, dev string) error {
	_, err := ctlCommand.getProcessInfo(ctlCommand.createRPCClient(), program)
	if err != nil {
		fmt.Printf("Not exist program %s\n", program)
		return err
	}
	url := fmt.Sprintf("%s/logtail/%s/%s", ctlCommand.getServerURL(), program, dev)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(ctlCommand.getUser(), ctlCommand.getPassword())
	client := http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	buf := make([]byte, 10240)
	for {
		n, err := resp.Body.Read(buf)
		if err != nil {
			return err
		}
		if dev == "stdout" {
			os.Stdout.Write(buf[0:n])
		} else {
			os.Stderr.Write(buf[0:n])
		}
	}
	return nil
}

// Execute check if the number of arguments is ok
func (wc *CmdCheckWrapperCommand) Execute(args []string) error {
	if len(args) < wc.leastNumArgs {
		err := fmt.Errorf("Invalid arguments.\nUsage: supervisord ctl %v", wc.usage)
		fmt.Printf("%v\n", err)
		return err
	}
	return wc.cmd.Execute(args)
}

func init() {
	ctlCmd, _ := parser.AddCommand("ctl",
		"Control a running daemon",
		"The ctl subcommand resembles supervisorctl command of original daemon.",
		&ctlCommand)
	ctlCmd.AddCommand("status",
		"show program status",
		"show all or some program status",
		&statusCommand)
	ctlCmd.AddCommand("start",
		"start programs",
		"start one or more programs",
		&startCommand)
	ctlCmd.AddCommand("stop",
		"stop programs",
		"stop one or more programs",
		&stopCommand)
	ctlCmd.AddCommand("restart",
		"restart programs",
		"restart one or more programs",
		&restartCommand)
	ctlCmd.AddCommand("shutdown",
		"shutdown supervisord",
		"shutdown supervisord",
		&shutdownCommand)
	ctlCmd.AddCommand("reload",
		"reload the programs",
		"reload the programs",
		&reloadCommand)
	ctlCmd.AddCommand("signal",
		"send signal to program",
		"send signal to program",
		&signalCommand)
	ctlCmd.AddCommand("pid",
		"get the pid of specified program",
		"get the pid of specified program",
		&pidCommand)
	ctlCmd.AddCommand("logtail",
		"get the standard output&standard error of the program",
		"get the standard output&standard error of the program",
		&logtailCommand)
	ctlCmd.AddCommand("exec",
		"enter program's namespace and run shell or command",
		"Like kubectl exec: -i/-t, optional -- then command. A lone sh/bash/ash/ksh attaches stdin automatically. Examples: ctl exec init -- sh ; ctl exec -it init -- /bin/sh ; ctl exec init -- ls -la",
		execWrapper)

}
