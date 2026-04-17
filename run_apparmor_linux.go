//go:build linux
// +build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/ochinchina/supervisord/util"
)

// Internal env keys set by supervisord process package for __run_container__ child only.
// Stripped from the final exec environ so the target (e.g. /init) does not inherit them.
const (
	envAppArmorProfile = "SUPERVISORD_APPARMOR_EXEC_PROFILE"
	envAppArmorRelaxed = "SUPERVISORD_APPARMOR_RELAXED"
	envTargetUID       = "SUPERVISORD_TARGET_UID"
	envTargetGID       = "SUPERVISORD_TARGET_GID"
)

func isTruthyEnv(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func procCmdlineForLog() string {
	s := strings.Join(os.Args, " ")
	if len(s) > 4000 {
		return s[:4000] + "…"
	}
	return s
}

func validateAppArmorProfileName(name string) error {
	if name == "" {
		return fmt.Errorf("empty apparmor profile name")
	}
	if len(name) > 300 {
		return fmt.Errorf("apparmor profile name too long")
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return fmt.Errorf("apparmor profile name has invalid control character")
		}
	}
	return nil
}

// writeAppArmorExecAttr queues an AppArmor profile for the next execve(2), using the
// kernel task security "exec" attribute (same mechanism as libapparmor aa_change_onexec).
//
// See: Linux Documentation/admin-guide/LSM/apparmor.rst ("exec" profile transitions).
// Typical payload is a single line: "exec <profile_name>\n"
func writeAppArmorExecAttr(profile string) error {
	if err := validateAppArmorProfileName(profile); err != nil {
		return err
	}
	payload := "exec " + profile + "\n"
	f, err := os.OpenFile("/proc/self/attr/exec", os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		logAppArmorExecAttrFailure(profile, payload, "open", err)
		return fmt.Errorf("open /proc/self/attr/exec: %w", err)
	}
	defer f.Close()
	n, err := f.WriteString(payload)
	if err != nil {
		logAppArmorExecAttrFailure(profile, payload, "write", err)
		return fmt.Errorf("write /proc/self/attr/exec: %w", err)
	}
	if n != len(payload) {
		logAppArmorExecAttrFailure(profile, payload, "short_write", fmt.Errorf("got %d want %d", n, len(payload)))
		return fmt.Errorf("short write /proc/self/attr/exec: %d/%d", n, len(payload))
	}
	return nil
}

// logAppArmorExecAttrFailure prints current-process details to stderr when attr/exec fails (triaging EACCES etc.).
func logAppArmorExecAttrFailure(profile, payload, phase string, cause error) {
	fmt.Fprintf(os.Stderr, "run_container: --- AppArmor /proc/self/attr/exec %s failed: %v\n", phase, cause)
	fmt.Fprintf(os.Stderr, "run_container: target_profile=%q payload=%q\n", profile, strings.TrimSpace(payload))
	fmt.Fprintf(os.Stderr, "run_container: pid=%d ppid=%d uid=%d gid=%d euid=%d egid=%d\n",
		os.Getpid(), os.Getppid(), os.Getuid(), os.Getgid(), os.Geteuid(), os.Getegid())
	if b, err := os.ReadFile("/proc/self/cmdline"); err == nil {
		cmd := strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
		if len(cmd) > 2000 {
			cmd = cmd[:2000] + "…"
		}
		fmt.Fprintf(os.Stderr, "run_container: cmdline: %s\n", cmd)
	} else {
		fmt.Fprintf(os.Stderr, "run_container: cmdline: (read err %v)\n", err)
	}
	if b, err := os.ReadFile("/proc/self/attr/current"); err == nil {
		fmt.Fprintf(os.Stderr, "run_container: /proc/self/attr/current: %q\n", strings.TrimSpace(string(b)))
	} else {
		fmt.Fprintf(os.Stderr, "run_container: /proc/self/attr/current: (read err %v)\n", err)
	}
	logProcSelfStatusPick(os.Stderr)
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) > 12 {
			lines = lines[:12]
		}
		fmt.Fprintf(os.Stderr, "run_container: /proc/self/cgroup (first lines):\n%s\n", strings.Join(lines, "\n"))
	}
	if target, err := os.Readlink("/proc/self/ns/pid"); err == nil {
		fmt.Fprintf(os.Stderr, "run_container: /proc/self/ns/pid -> %s\n", target)
	}
	if target, err := os.Readlink("/proc/self/ns/user"); err == nil {
		fmt.Fprintf(os.Stderr, "run_container: /proc/self/ns/user -> %s\n", target)
	}
	fmt.Fprintf(os.Stderr, "run_container: --- end AppArmor attr/exec diagnostics\n")
}

var procStatusPrefixes = []string{
	"Name:", "State:", "Pid:", "PPid:", "TracerPid:", "Uid:", "Gid:", "Groups:",
	"NSpid:", "NStgid:", "NSpgid:", "NSsid:", "Seccomp:", "NoNewPrivs:",
	"CapInh:", "CapPrm:", "CapEff:", "CapBnd:", "CapAmb:",
}

func logProcSelfStatusPick(w *os.File) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		fmt.Fprintf(w, "run_container: /proc/self/status: (open err %v)\n", err)
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	const maxScan = 512 * 1024
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, maxScan)
	for sc.Scan() {
		line := sc.Text()
		for _, pref := range procStatusPrefixes {
			if strings.HasPrefix(line, pref) {
				fmt.Fprintf(w, "run_container: status %s\n", line)
				break
			}
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(w, "run_container: /proc/self/status: (scan err %v)\n", err)
	}
}

// prepareAppArmorExecTransition reads profile/relaxed flags from the environment (injected
// by supervisord for container_run programs) and writes /proc/self/attr/exec when requested.
//
// skipWhenPID1: when true and the caller is PID 1 in its namespace, skip the transition. The
// writing /proc/self/attr/exec from PID 1 in a fresh PID namespace is often denied or skipped;
// non-init targets instead use __run_gate_exec__ (forked child, pid != 1) before the final exec.
func prepareAppArmorExecTransition(skipWhenPID1 bool) error {
	prof := strings.TrimSpace(os.Getenv(envAppArmorProfile))
	if prof == "" {
		fmt.Fprintln(os.Stderr, "run_container: no apparmor profile requested (SUPERVISORD_APPARMOR_EXEC_PROFILE is empty)")
		return nil
	}
	if skipWhenPID1 && os.Getpid() == 1 {
		log.WithFields(log.Fields{
			"profile": prof,
			"cmdline": procCmdlineForLog(),
		}).Info("run_container: skip AppArmor exec transition on /proc/self/attr/exec (pid=1)")
		return nil
	}
	if !util.AppArmorExecTransitionAvailable() {
		fmt.Fprintf(os.Stderr, "run_container: AppArmor exec transition unavailable; ignoring %s=%q (%s)\n", envAppArmorProfile, prof, appArmorExecUnavailableReason())
		return nil
	}
	relaxed := isTruthyEnv(os.Getenv(envAppArmorRelaxed))
	if err := writeAppArmorExecAttr(prof); err != nil {
		if relaxed {
			fmt.Fprintf(os.Stderr, "run_container: apparmor exec transition skipped (relaxed): %v\n", err)
			return nil
		}
		return err
	}
	fmt.Fprintf(os.Stderr, "run_container: apparmor exec transition queued for profile %q\n", prof)
	return nil
}

func appArmorExecUnavailableReason() string {
	if _, err := os.Stat("/sys/kernel/security/apparmor"); err != nil {
		if _, err2 := os.Stat("/sys/module/apparmor"); err2 != nil {
			return "apparmor subsystem paths missing (/sys/kernel/security/apparmor and /sys/module/apparmor)"
		}
	}
	if b, err := os.ReadFile("/sys/module/apparmor/parameters/enabled"); err == nil && strings.TrimSpace(string(b)) == "N" {
		return "kernel apparmor module disabled (enabled=N)"
	}
	if _, err := os.OpenFile("/proc/self/attr/exec", os.O_WRONLY|os.O_TRUNC, 0); err != nil {
		return fmt.Sprintf("cannot open /proc/self/attr/exec for write: %v", err)
	}
	return "unknown reason"
}

func filterSupervisordInternalEnv(env []string) []string {
	drop := map[string]struct{}{
		envAppArmorProfile: {},
		envAppArmorRelaxed: {},
		envTargetUID:       {},
		envTargetGID:       {},
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		k, _, ok := strings.Cut(e, "=")
		if !ok {
			out = append(out, e)
			continue
		}
		if _, d := drop[k]; d {
			continue
		}
		out = append(out, e)
	}
	return out
}
