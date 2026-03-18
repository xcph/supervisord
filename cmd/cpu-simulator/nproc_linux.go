//go:build linux
// +build linux

package main

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// getNproc 返回当前进程可用的 CPU 数量上限（nproc 原理）。
// 实现逻辑：读取 cgroup v2 cpu.max 或 cgroup v1 cpu.cfs_quota_us/cpu.cfs_period_us，
// 计算 ceil(quota/period) 作为 cgroup 限制的 CPU 数；若无限制则使用 runtime.NumCPU()。
// 不调用 nproc 命令行。
func getNproc() int {
	if n := getCgroupCPUQuota(); n >= 0 {
		return n
	}
	return runtime.NumCPU()
}

// getCgroupCPUQuota 从 cgroup 读取 CPU 配额，返回 ceil(quota/period)，无限制时返回 -1。
func getCgroupCPUQuota() int {
	if n := getCgroupV2CPUQuota(); n >= 0 {
		return n
	}
	if n := getCgroupV1CPUQuota(); n >= 0 {
		return n
	}
	return -1
}

// getCgroupV2CPUQuota 读取 cgroup v2 cpu.max，格式 "quota period" 或 "max period"。
func getCgroupV2CPUQuota() int {
	if !isCgroupV2() {
		return -1
	}
	groupPath := getCgroupV2Path()
	if groupPath == "" {
		return -1
	}
	cpuMaxPath := filepath.Join("/sys/fs/cgroup", groupPath, "cpu.max")
	data, err := os.ReadFile(cpuMaxPath)
	if err != nil {
		return -1
	}
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) == 0 || len(fields) > 2 {
		return -1
	}
	if fields[0] == "max" {
		return -1
	}
	quota, err := strconv.Atoi(fields[0])
	if err != nil || quota <= 0 {
		return -1
	}
	period := 100000
	if len(fields) == 2 {
		period, err = strconv.Atoi(fields[1])
		if err != nil || period <= 0 {
			return -1
		}
	}
	n := int(math.Ceil(float64(quota) / float64(period)))
	if n < 1 {
		n = 1
	}
	return n
}

func isCgroupV2() bool {
	// 检查 /sys/fs/cgroup 是否为 cgroup2：mountinfo 中该挂载点的 fstype 为 cgroup2
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		mountPoint := fields[4]
		if mountPoint != "/sys/fs/cgroup" {
			continue
		}
		// mountinfo: id parent dev root mount opts - fstype source superopts
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" && i+2 < len(fields) {
				if fields[i+1] == "cgroup2" {
					return true
				}
				break
			}
		}
		break
	}
	return false
}

func getCgroupV2Path() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" {
			return strings.TrimPrefix(parts[2], "/")
		}
	}
	return ""
}

// getCgroupV1CPUQuota 读取 cgroup v1 cpu.cfs_quota_us 和 cpu.cfs_period_us。
func getCgroupV1CPUQuota() int {
	cpuPath, err := getCgroupV1CPUPath()
	if err != nil || cpuPath == "" {
		return -1
	}
	quotaData, err := os.ReadFile(filepath.Join(cpuPath, "cpu.cfs_quota_us"))
	if err != nil {
		return -1
	}
	quota, err := strconv.Atoi(strings.TrimSpace(string(quotaData)))
	if err != nil || quota <= 0 {
		return -1 // -1 表示无限制
	}
	periodData, err := os.ReadFile(filepath.Join(cpuPath, "cpu.cfs_period_us"))
	if err != nil {
		return -1
	}
	period, err := strconv.Atoi(strings.TrimSpace(string(periodData)))
	if err != nil || period <= 0 {
		return -1
	}
	n := int(math.Ceil(float64(quota) / float64(period)))
	if n < 1 {
		n = 1
	}
	return n
}

func getCgroupV1CPUPath() (string, error) {
	cgroupData, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	mountInfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	// 解析 cgroup 获取 cpu 子系统的 path（如 /kubepods.slice/...）
	var cpuCgroupPath string
	for _, line := range strings.Split(string(cgroupData), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, sub := range strings.Split(parts[1], ",") {
			if sub == "cpu" {
				cpuCgroupPath = strings.TrimPrefix(parts[2], "/")
				break
			}
		}
		if cpuCgroupPath != "" {
			break
		}
	}
	if cpuCgroupPath == "" {
		return "", nil
	}
	// 解析 mountinfo：格式 ... mount_point opts - fstype source superopts
	// superopts 如 "rw,cpu,cpuacct" 包含子系统名
	for _, line := range strings.Split(string(mountInfo), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		mountPoint := fields[4]
		superOpts := fields[len(fields)-1]
		for _, opt := range strings.Split(superOpts, ",") {
			if opt == "cpu" {
				return filepath.Join(mountPoint, cpuCgroupPath), nil
			}
		}
	}
	return "", nil
}
