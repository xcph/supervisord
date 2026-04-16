//go:build linux

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	nodeagentv1 "github.com/xcph/cloudphone-nodeagent-api/pkg/apiv1"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/utils/cpuset"
)

// WaitForKoordletBeforeAndroidCpusetSetup blocks until koordlet has applied the m+n cpuset
// to this cgroup (strict: matches koordlet checkpoint merged dedicated∪shared), or until timeout.
//
// Enable when any of:
//   - KOORDINATOR_QOS_CLASS is LSR or LSE, or
//   - SUPERVISORD_WAIT_KOORDLET_CPUSET is 1/true
//
// Disable with SUPERVISORD_WAIT_KOORDLET_CPUSET=0/false.
//
// Strict mode (default for LSR/LSE): compare cgroup cpuset.cpus to the expected m+n set using
// k8s.io/utils/cpuset canonical form. Expected value is resolved in order:
//   1) KOORDINATOR_EXPECT_CPUSET — explicit list (e.g. "1,2-3,5-7")
//   2) cloudphone-node-agent RPC GetKoordletMPlusNCpuset (reads host checkpoint; no koordlet mount in Pod)
//   3) Optional: KOORDLET_CPUSET_CHECKPOINT_DIR — local read (e.g. dev-only mount)
//
// Non-strict (SUPERVISORD_KOORDLET_CPUSET_STRICT=0): fall back to "fewer CPUs than online" heuristic.
//
// KOORDLET_CPUSET_WAIT_TIMEOUT_SEC — default 120.
func WaitForKoordletBeforeAndroidCpusetSetup() {
	if !shouldWaitKoordletCpuset() {
		return
	}
	timeout := 120 * time.Second
	if s := strings.TrimSpace(os.Getenv("KOORDLET_CPUSET_WAIT_TIMEOUT_SEC")); s != "" {
		if sec, err := strconv.Atoi(s); err == nil && sec > 0 {
			timeout = time.Duration(sec) * time.Second
		}
	}
	strict := koordletCpusetStrict()
	cpusetFile := containerCpusetCpusPath()
	if cpusetFile == "" {
		log.Warn("koordlet cpuset wait: cannot resolve cpuset cgroup path, skip wait")
		return
	}
	online, err := readSysCPUOnlineList()
	if err != nil {
		log.WithError(err).Warn("koordlet cpuset wait: read online CPUs failed, skip wait")
		return
	}
	deadline := time.Now().Add(timeout)
	poll := 200 * time.Millisecond
	log.Infof("koordlet cpuset wait: strict=%v timeout=%v file=%s", strict, timeout, cpusetFile)

	var last string
	stable := 0
	for time.Now().Before(deadline) {
		expected, expOK, expErr := resolveExpectedMPlusN(strict)
		if expErr != nil {
			log.WithError(expErr).Debug("koordlet cpuset wait: resolve expected")
		}
		cur, err := readTrimFile(cpusetFile)
		if err != nil {
			log.WithError(err).Debug("koordlet cpuset wait: read cpuset.cpus")
			time.Sleep(poll)
			continue
		}
		var ok bool
		if strict && expOK {
			ok, err = cpusetCanonicalEqual(cur, expected)
			if err != nil {
				log.WithError(err).Debug("koordlet cpuset wait: compare")
				ok = false
			}
		} else if strict && !expOK {
			// Wait for checkpoint / explicit env to appear
			ok = false
		} else {
			ok = cpusetWaitSatisfiedLoose(cur, online)
		}
		if ok {
			if cur == last {
				stable++
			} else {
				last = cur
				stable = 1
			}
			if stable >= 2 {
				if strict && expOK {
					log.Infof("koordlet cpuset wait: m+n match stable cpuset.cpus=%q (expected %q)", cur, expected)
				} else {
					log.Infof("koordlet cpuset wait: satisfied stable cpuset.cpus=%q", cur)
				}
				return
			}
		} else {
			stable = 0
		}
		time.Sleep(poll)
	}
	log.Warnf("koordlet cpuset wait: timeout after %v, last=%q — proceeding anyway", timeout, last)
}

func koordletCpusetStrict() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SUPERVISORD_KOORDLET_CPUSET_STRICT")))
	if v == "0" || v == "false" || v == "no" {
		return false
	}
	if v == "1" || v == "true" || v == "yes" {
		return true
	}
	qos := strings.ToUpper(strings.TrimSpace(os.Getenv("KOORDINATOR_QOS_CLASS")))
	return qos == "LSR" || qos == "LSE"
}

func shouldWaitKoordletCpuset() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SUPERVISORD_WAIT_KOORDLET_CPUSET")))
	if v == "0" || v == "false" || v == "no" {
		return false
	}
	if v == "1" || v == "true" || v == "yes" {
		return true
	}
	qos := strings.ToUpper(strings.TrimSpace(os.Getenv("KOORDINATOR_QOS_CLASS")))
	return qos == "LSR" || qos == "LSE"
}

func containerCpusetCpusPath() string {
	root, cgPath := findCpusetPath("/proc/self/cgroup")
	if root == "" || cgPath == "" {
		return ""
	}
	cgRel := strings.TrimPrefix(cgPath, "/")
	base := filepath.Join(root, cgRel)
	return filepath.Join(base, "cpuset.cpus")
}

func readSysCPUOnlineList() ([]int, error) {
	b, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return nil, err
	}
	return parseCPUList(strings.TrimSpace(string(b))), nil
}

func readTrimFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// cpusetWaitSatisfiedLoose: fewer bound CPUs than online (legacy non-strict).
func cpusetWaitSatisfiedLoose(current string, online []int) bool {
	cur := parseCPUList(current)
	if len(cur) == 0 || len(online) == 0 {
		return false
	}
	return len(cur) < len(online)
}

func parseCPUList(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '-'); i >= 0 {
			var lo, hi int
			_, err := fmt.Sscanf(part, "%d-%d", &lo, &hi)
			if err != nil || lo > hi {
				continue
			}
			for c := lo; c <= hi; c++ {
				out = append(out, c)
			}
			continue
		}
		var x int
		if _, err := fmt.Sscanf(part, "%d", &x); err == nil {
			out = append(out, x)
		}
	}
	return out
}

func cpusetCanonicalString(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty cpuset")
	}
	set, err := cpuset.Parse(s)
	if err != nil {
		return "", err
	}
	return set.String(), nil
}

func cpusetCanonicalEqual(a, b string) (bool, error) {
	ca, err := cpusetCanonicalString(a)
	if err != nil {
		return false, err
	}
	cb, err := cpusetCanonicalString(b)
	if err != nil {
		return false, err
	}
	return ca == cb, nil
}

type mplusnCheckpoint struct {
	Entries map[string]struct {
		Dedicated string `json:"dedicated"`
		Shared    string `json:"shared"`
	} `json:"entries"`
}

var (
	nodeAgentGRPCMu sync.Mutex
	nodeAgentGRPC   *grpc.ClientConn
)

func resolveExpectedMPlusN(strict bool) (expected string, ok bool, err error) {
	if ex := strings.TrimSpace(os.Getenv("KOORDINATOR_EXPECT_CPUSET")); ex != "" {
		can, err := cpusetCanonicalString(ex)
		if err != nil {
			return "", false, err
		}
		return can, true, nil
	}
	sock := strings.TrimSpace(os.Getenv("CLOUDPHONE_NODE_AGENT_SOCKET"))
	if sock != "" {
		s, err := fetchExpectedCpusetFromNodeAgent(sock)
		if err != nil {
			return "", false, err
		}
		if s != "" {
			return s, true, nil
		}
	}
	dir := strings.TrimSpace(os.Getenv("KOORDLET_CPUSET_CHECKPOINT_DIR"))
	if dir == "" {
		dir = "/var/lib/koordlet"
	}
	uid := strings.TrimSpace(os.Getenv("POD_UID"))
	if uid == "" {
		if strict {
			return "", false, fmt.Errorf("POD_UID unset and node-agent did not return m+n (set KOORDINATOR_EXPECT_CPUSET or CLOUDPHONE_NODE_AGENT_SOCKET)")
		}
		return "", false, nil
	}
	merged, rerr := readMergedMPlusNCheckpoint(dir)
	if rerr != nil {
		return "", false, rerr
	}
	e, found := merged[uid]
	if !found {
		return "", false, nil
	}
	mergedStr := mergeDedicatedShared(e.Dedicated, e.Shared)
	if mergedStr == "" {
		return "", false, fmt.Errorf("empty m+n for pod %s", uid)
	}
	can, err := cpusetCanonicalString(mergedStr)
	if err != nil {
		return "", false, err
	}
	return can, true, nil
}

func fetchExpectedCpusetFromNodeAgent(sock string) (string, error) {
	uid := strings.TrimSpace(os.Getenv("POD_UID"))
	if uid == "" {
		return "", nil
	}
	nodeAgentGRPCMu.Lock()
	if nodeAgentGRPC == nil {
		c, err := grpc.NewClient(
			"unix://"+sock,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			nodeAgentGRPCMu.Unlock()
			return "", err
		}
		nodeAgentGRPC = c
	}
	cli := nodeagentv1.NewNodeAgentClient(nodeAgentGRPC)
	nodeAgentGRPCMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := cli.GetKoordletMPlusNCpuset(ctx, &nodeagentv1.GetKoordletMPlusNCpusetRequest{PodUid: uid})
	if err != nil {
		return "", err
	}
	if !resp.GetOk() {
		return "", nil
	}
	ex := strings.TrimSpace(resp.GetExpectedCpuset())
	if ex == "" {
		return "", nil
	}
	return cpusetCanonicalString(ex)
}

func mergeDedicatedShared(d, sh string) string {
	d = strings.TrimSpace(d)
	sh = strings.TrimSpace(sh)
	if d == "" && sh == "" {
		return ""
	}
	if d == "" {
		return sh
	}
	if sh == "" {
		return d
	}
	setA, errA := cpuset.Parse(d)
	setB, errB := cpuset.Parse(sh)
	if errA != nil || errB != nil {
		return d + "," + sh
	}
	return setA.Union(setB).String()
}

func readMergedMPlusNCheckpoint(dir string) (map[string]struct {
	Dedicated string `json:"dedicated"`
	Shared    string `json:"shared"`
}, error) {
	base := filepath.Join(dir, "cpuset_m_plus_n_state")
	var paths []string
	if st, err := os.Stat(base); err == nil && !st.IsDir() {
		paths = append(paths, base)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "cpuset_m_plus_n_state_numa_*"))
	if err != nil {
		return nil, err
	}
	paths = append(paths, matches...)

	out := make(map[string]struct {
		Dedicated string `json:"dedicated"`
		Shared    string `json:"shared"`
	})
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var raw mplusnCheckpoint
		if json.Unmarshal(data, &raw) != nil || raw.Entries == nil {
			continue
		}
		for uid, e := range raw.Entries {
			if _, exists := out[uid]; !exists {
				out[uid] = e
			}
		}
	}
	// No files yet (koordlet not written): empty map, no error — caller keeps polling.
	return out, nil
}
