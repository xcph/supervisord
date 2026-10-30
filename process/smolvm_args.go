package process

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

type containerRunSpec struct {
	desired     string
	image       string
	stateDir    string
	machineName string
	adbRelay    string
	cpus        int
	mem         int
	publish     string
	pauseFile   string
	resumeFile  string
	runtimeFile string
}

func normalizeRunType(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), "smolvm") {
		return "smolvm"
	}
	return "namespace"
}

func chooseContainerRun(desired string, fellBack bool, preflight error) (use bool, actual, reason string) {
	if normalizeRunType(desired) != "smolvm" {
		return false, "namespace", ""
	}
	if fellBack {
		return false, "namespace", "smolvm already fell back in this start"
	}
	if preflight != nil {
		return false, "namespace", preflight.Error()
	}
	return true, "smolvm", ""
}

func (p *Process) containerRunSpec() containerRunSpec {
	overlay := p.config.GetEnvFromFiles("envFilesOverlay")
	desired := firstEnv(envPair(overlay, "CONTAINER_RUN_TYPE"), os.Getenv("CONTAINER_RUN_TYPE"), p.config.GetString("container_run_type", "namespace"))
	image := firstEnv(envPair(overlay, "CONTAINER_RUN_IMAGE"), os.Getenv("CONTAINER_RUN_IMAGE"))
	stateDir := firstEnv(
		envPair(overlay, "CONTAINER_RUN_STATE_DIR"),
		os.Getenv("CONTAINER_RUN_STATE_DIR"),
		p.config.GetString("container_run_state_dir", ""),
	)
	machineName := firstEnv(
		envPair(overlay, "CONTAINER_RUN_SMOLVM_NAME"),
		os.Getenv("CONTAINER_RUN_SMOLVM_NAME"),
		p.config.GetString("container_run_smolvm_name", ""),
		p.GetName(),
	)
	adb := firstEnv(envPair(overlay, "SMOLVM_ADB_RELAY"), os.Getenv("SMOLVM_ADB_RELAY"), p.config.GetString("smolvm_adb_relay", "192.168.127.1:5555"))
	cpus := atoiDefault(firstEnv(envPair(overlay, "CONTAINER_RUN_CPUS"), os.Getenv("CONTAINER_RUN_CPUS"), p.config.GetString("container_run_cpus", "")), 2)
	mem := atoiDefault(firstEnv(envPair(overlay, "CONTAINER_RUN_MEM"), os.Getenv("CONTAINER_RUN_MEM"), p.config.GetString("container_run_mem", "")), 2048)
	publish := firstEnv(envPair(overlay, "CONTAINER_RUN_PUBLISH"), os.Getenv("CONTAINER_RUN_PUBLISH"), p.config.GetString("container_run_publish", "127.0.0.1:18889:18889"))
	pauseFile := firstEnv(p.config.GetString("container_run_pause_file", ""), "pause")
	resumeFile := firstEnv(p.config.GetString("container_run_resume_file", ""), "resume")
	runtimeFile := firstEnv(p.config.GetString("container_run_runtime_file", ""), "runtime.json")
	return containerRunSpec{
		desired:     normalizeRunType(desired),
		image:       strings.TrimSpace(image),
		stateDir:    stateDir,
		machineName: machineName,
		adbRelay:    adb,
		cpus:        cpus,
		mem:         mem,
		publish:     publish,
		pauseFile:   pauseFile,
		resumeFile:  resumeFile,
		runtimeFile: runtimeFile,
	}
}

func (p *Process) containerRunStateDir() string {
	return p.containerRunSpec().stateDir
}

// prepareSmolvm replaces args with a smolvm launch when container_run_type=smolvm
// (or CONTAINER_RUN_TYPE) and the desired type can actually start. Returns false
// when the caller should keep the namespace wrapper.
func (p *Process) prepareSmolvm(args *[]string) bool {
	p.stopADBRelay()
	p.launchedSmolvm = false
	spec := p.containerRunSpec()
	if spec.desired != "smolvm" {
		return false
	}
	var preflight error
	if !p.smolvmFallback {
		preflight = smolvmPreflight(spec.image)
	}
	use, actual, reason := chooseContainerRun(spec.desired, p.smolvmFallback, preflight)
	if p.smolvmFallback && p.smolvmFallbackReason != "" {
		reason = p.smolvmFallbackReason
	}
	if use && containerResumeRequested(spec.stateDir, spec.resumeFile) {
		*args = smolvmResumeArgv(spec.machineName)
		_ = os.Remove(filepath.Join(spec.stateDir, spec.resumeFile))
	} else if use {
		built, err := smolvmRunArgv(spec, *args)
		if err != nil {
			use = false
			actual = "namespace"
			reason = err.Error()
			p.smolvmFallback = true
			p.smolvmFallbackReason = reason
		} else {
			*args = built
		}
	}
	if use {
		actual = "smolvm"
		p.launchedSmolvm = true
		p.startADBRelay(spec.adbRelay)
	}
	p.writeContainerRuntime(spec, spec.desired, actual, reason)
	return use
}

func (p *Process) noteSmolvmStartFailure(err error) bool {
	if !p.launchedSmolvm || p.smolvmFallback || err == nil {
		return false
	}
	p.smolvmFallback = true
	p.smolvmFallbackReason = "smolvm failed to start: " + err.Error()
	p.writeContainerRuntime(p.containerRunSpec(), "smolvm", "namespace", p.smolvmFallbackReason)
	p.launchedSmolvm = false
	p.stopADBRelay()
	log.WithFields(log.Fields{"program": p.GetName(), "reason": p.smolvmFallbackReason}).Warn("smolvm failed; next attempt uses the namespace")
	return true
}

func smolvmResumeArgv(machineName string) []string {
	if strings.TrimSpace(machineName) == "" {
		machineName = "program"
	}
	return []string{"smolvm", "machine", "resume", "--name", machineName}
}

func smolvmRunArgv(spec containerRunSpec, command []string) ([]string, error) {
	image := strings.TrimSpace(spec.image)
	if image == "" {
		return nil, errImageRequired
	}
	cpus := spec.cpus
	if cpus < 1 {
		cpus = 2
	}
	mem := spec.mem
	if mem < 256 {
		mem = 2048
	}
	machineName := strings.TrimSpace(spec.machineName)
	if machineName == "" {
		machineName = "program"
	}
	args := []string{
		"smolvm", "machine", "run",
		"--name", machineName,
		"--net",
		"--net-backend", "virtio-net",
		"--cpus", strconv.Itoa(cpus),
		"--mem", strconv.Itoa(mem),
	}
	if stateDir := strings.TrimSpace(spec.stateDir); stateDir != "" {
		args = append(args, "-v", stateDir+":"+stateDir)
	}
	if publish := strings.TrimSpace(spec.publish); publish != "" {
		args = append(args, "-p", publish)
	}
	args = append(args, "--image", image, "--")
	return append(args, command...), nil
}

var errImageRequired = errString("CONTAINER_RUN_IMAGE is empty")

type errString string

func (e errString) Error() string { return string(e) }

func (p *Process) writeContainerRuntime(spec containerRunSpec, desired, actual, reason string) {
	dir := strings.TrimSpace(spec.stateDir)
	if dir == "" {
		return
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return
	}
	body, err := json.Marshal(map[string]string{
		"desired": desired,
		"actual":  actual,
		"reason":  reason,
		"at":      time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	name := spec.runtimeFile
	if name == "" {
		name = "runtime.json"
	}
	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		log.WithFields(log.Fields{"program": p.GetName(), "err": err}).Warn("write container runtime file")
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.WithFields(log.Fields{"program": p.GetName(), "err": err}).Warn("rename container runtime file")
	}
}

func (p *Process) startADBRelay(addr string) {
	p.stopADBRelay()
	if strings.TrimSpace(addr) == "" {
		addr = "192.168.127.1:5555"
	}
	stop := make(chan struct{})
	p.relayStop = stop
	go runADBRelay(addr, "127.0.0.1:5555", stop)
}

func (p *Process) stopADBRelay() {
	if p.relayStop == nil {
		return
	}
	close(p.relayStop)
	p.relayStop = nil
}

func containerPauseRequested(dir, pauseFile string) bool {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(pauseFile) == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, pauseFile))
	return err == nil
}

func containerResumeRequested(dir, resumeFile string) bool {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(resumeFile) == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, resumeFile))
	return err == nil
}

func envPair(pairs []string, key string) string {
	prefix := key + "="
	for _, item := range pairs {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimSpace(item[len(prefix):])
		}
	}
	return ""
}

func firstEnv(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func atoiDefault(v string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}
