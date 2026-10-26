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

const gatewayMachineName = "openclaw-gateway"

type gatewayRunSpec struct {
	desired  string
	image    string
	stateDir string
	adbRelay string
	cpus     int
	mem      int
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

func (p *Process) gatewayRunSpec() gatewayRunSpec {
	overlay := p.config.GetEnvFromFiles("envFilesOverlay")
	desired := firstEnv(envPair(overlay, "CONTAINER_RUN_TYPE"), os.Getenv("CONTAINER_RUN_TYPE"), p.config.GetString("container_run_type", "namespace"))
	image := firstEnv(envPair(overlay, "CONTAINER_RUN_IMAGE"), os.Getenv("CONTAINER_RUN_IMAGE"))
	stateDir := firstEnv(envPair(overlay, "OPENCLAW_STATE_DIR"), os.Getenv("OPENCLAW_STATE_DIR"), "/home/node/.openclaw")
	adb := firstEnv(envPair(overlay, "SMOLVM_ADB_RELAY"), os.Getenv("SMOLVM_ADB_RELAY"), "192.168.127.1:5555")
	cpus := atoiDefault(firstEnv(envPair(overlay, "CONTAINER_RUN_CPUS"), os.Getenv("CONTAINER_RUN_CPUS")), 2)
	mem := atoiDefault(firstEnv(envPair(overlay, "CONTAINER_RUN_MEM"), os.Getenv("CONTAINER_RUN_MEM")), 2048)
	return gatewayRunSpec{
		desired:  normalizeRunType(desired),
		image:    strings.TrimSpace(image),
		stateDir: stateDir,
		adbRelay: adb,
		cpus:     cpus,
		mem:      mem,
	}
}

func (p *Process) gatewayStateDir() string {
	return p.gatewayRunSpec().stateDir
}

// prepareSmolvm replaces args with a smolvm launch when this program is the gateway
// and the desired type can actually start. It returns false when the caller should
// keep the namespace wrapper.
func (p *Process) prepareSmolvm(args *[]string) bool {
	if p.GetName() != gatewayMachineName {
		return false
	}
	p.stopADBRelay()
	p.launchedSmolvm = false
	spec := p.gatewayRunSpec()
	var preflight error
	if spec.desired == "smolvm" && !p.smolvmFallback {
		preflight = smolvmPreflight(spec.image)
	}
	use, actual, reason := chooseContainerRun(spec.desired, p.smolvmFallback, preflight)
	if p.smolvmFallback && p.smolvmFallbackReason != "" {
		reason = p.smolvmFallbackReason
	}
	if use && gatewayResumeRequested(spec.stateDir) {
		*args = smolvmResumeArgv()
		_ = os.Remove(filepath.Join(spec.stateDir, "gateway.resume"))
	} else if use {
		built, err := smolvmRunArgv(spec.image, spec.cpus, spec.mem, spec.stateDir, *args)
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
	p.writeGatewayRuntime(spec.desired, actual, reason)
	return use
}

func (p *Process) noteSmolvmStartFailure(err error) bool {
	if !p.launchedSmolvm || p.smolvmFallback || err == nil {
		return false
	}
	p.smolvmFallback = true
	p.smolvmFallbackReason = "smolvm failed to start: " + err.Error()
	p.writeGatewayRuntime("smolvm", "namespace", p.smolvmFallbackReason)
	p.launchedSmolvm = false
	p.stopADBRelay()
	log.WithFields(log.Fields{"program": p.GetName(), "reason": p.smolvmFallbackReason}).Warn("smolvm failed; next attempt uses the namespace")
	return true
}

func smolvmResumeArgv() []string {
	return []string{"smolvm", "machine", "resume", "--name", gatewayMachineName}
}

func smolvmRunArgv(image string, cpus, mem int, stateDir string, command []string) ([]string, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return nil, errImageRequired
	}
	if cpus < 1 {
		cpus = 2
	}
	if mem < 256 {
		mem = 2048
	}
	if strings.TrimSpace(stateDir) == "" {
		stateDir = "/home/node/.openclaw"
	}
	args := []string{
		"smolvm", "machine", "run",
		"--name", gatewayMachineName,
		"--net",
		"--net-backend", "virtio-net",
		"--cpus", strconv.Itoa(cpus),
		"--mem", strconv.Itoa(mem),
		"-v", stateDir + ":" + stateDir,
		"-p", "127.0.0.1:18889:18889",
		"--image", image,
		"--",
	}
	return append(args, command...), nil
}

var errImageRequired = errString("CONTAINER_RUN_IMAGE is empty")

type errString string

func (e errString) Error() string { return string(e) }

func (p *Process) writeGatewayRuntime(desired, actual, reason string) {
	dir := p.gatewayStateDir()
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
	path := filepath.Join(dir, "gateway-runtime.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		log.WithFields(log.Fields{"program": p.GetName(), "err": err}).Warn("write gateway-runtime.json")
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.WithFields(log.Fields{"program": p.GetName(), "err": err}).Warn("rename gateway-runtime.json")
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

func gatewayPauseRequested(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "gateway.pause"))
	return err == nil
}

func gatewayResumeRequested(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "gateway.resume"))
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
