// +build linux

package process

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Android cpuset cgroups that redroid init expects (see libprocessgroup/sched_policy.cpp).
var androidCpusetDirs = []string{"foreground", "system-background", "background", "top-app", "restricted", "foreground_window"}

// joinForegroundCpusetBeforeFork adds the current process to /dev/cpuset/foreground
// before forking a container_run child. Also creates all Android cpuset cgroups
// (foreground, system-background, background, top-app) so redroid init won't get ENOSPC.
// Best-effort; errors are ignored.
func joinForegroundCpusetBeforeFork() {
	cpusetRoot, cgPath := findCpusetPath("/proc/self/cgroup")
	if cpusetRoot == "" || cgPath == "" {
		return
	}
	cgRel := strings.TrimPrefix(cgPath, "/")
	base := filepath.Join(cpusetRoot, cgRel)
	// Set cgroup.clone_children=1 on cpuset root and on our cgroup
	for _, d := range []string{cpusetRoot, base} {
		if f := filepath.Join(d, "cgroup.clone_children"); pathExists(f) {
			_ = os.WriteFile(f, []byte("1"), 0)
		}
	}
	cpus, _ := os.ReadFile(filepath.Join(base, "cpuset.cpus"))
	mems, _ := os.ReadFile(filepath.Join(base, "cpuset.mems"))
	if len(cpus) == 0 {
		cpus = []byte("0")
	}
	if len(mems) == 0 {
		mems = []byte("0")
	}
	// Create all Android cpuset cgroups with cpuset.cpus/mems
	for _, name := range androidCpusetDirs {
		dir := filepath.Join(base, name)
		_ = os.MkdirAll(dir, 0755)
		_ = os.WriteFile(filepath.Join(dir, "cpuset.cpus"), cpus, 0)
		_ = os.WriteFile(filepath.Join(dir, "cpuset.mems"), mems, 0)
	}
	pid := os.Getpid()
	for _, tasksFile := range []string{"tasks", "cgroup.procs"} {
		p := filepath.Join(base, "foreground", tasksFile)
		if pathExists(p) && writeFile(p, strconv.Itoa(pid)+"\n") {
			break
		}
	}
}

func findCpusetPath(cgroupFile string) (string, string) {
	data, err := os.ReadFile(cgroupFile)
	if err != nil {
		return "", ""
	}
	var candidates []string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := sc.Text()
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		controllers, path := parts[1], parts[2]
		if path == "" || path == "/" {
			continue
		}
		if strings.Contains(controllers, "cpuset") {
			candidates = append([]string{path}, candidates...) // prefer cpuset
		} else if controllers == "" {
			candidates = append(candidates, path) // cgroup v2 unified
		}
	}
	for _, cgPath := range candidates {
		for _, root := range []string{"/sys/fs/cgroup/cpuset", "/sys/fs/cgroup"} {
			p := filepath.Join(root, strings.TrimPrefix(cgPath, "/"))
			if pathExists(filepath.Join(p, "cpuset.cpus")) || pathExists(filepath.Join(p, "cpuset.cpus.effective")) {
				return root, cgPath
			}
		}
	}
	return "", ""
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func writeFile(path, content string) bool {
	return os.WriteFile(path, []byte(content), 0) == nil
}
