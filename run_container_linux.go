//go:build linux
// +build linux

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var procfsSimulatorPaths = []string{"/shared/supervisord/procfs-simulator", "/usr/local/bin/procfs-simulator"}

const runContainerMagic = "__run_container__"
const runHelperMagic = "__run_helper__"

// CNI bootstrap: Android netd may flush eth0 IPv4/routes after boot; we snapshot what the CNI
// programmed (Cilium: /32 + default via x.x.x.2) and restore periodically. Calico uses 169.254.1.1.
const cniBootstrapDir = "/shared/cni-bootstrap"

// handleRunHelper runs when the process was invoked as:
//
//	supervisord __run_helper__ <init args...>
//
// Helper 进程由 run_container fork，在 init 启动后执行延迟任务（cpuset、proc-hide、tombstones）。
func handleRunHelper() bool {
	if len(os.Args) < 3 || os.Args[1] != runHelperMagic {
		return false
	}
	runHelperDelayedTasks()
	return true
}

// handleRunContainer runs when the process was invoked as:
//
//	supervisord __run_container__ <target> [args...]
//
// The process is the child forked with container namespaces (PID, mount, cgroup, UTS, IPC).
// Network ns is excluded by default so redroid netd can use iptables (pod network).
func handleRunContainer() bool {
	if len(os.Args) < 3 || os.Args[1] != runContainerMagic {
		return false
	}
	target := os.Args[2]
	argv := os.Args[2:]

	// Make mount namespace private so our unmounts don't propagate to supervisord (kubectl exec needs /dev/pts).
	_ = unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
	// Mount fresh /proc for the new PID namespace (we are PID 1 here).
	_ = unix.Unmount("/proc", unix.MNT_DETACH)
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		fmt.Fprintf(os.Stderr, "run_container: mount /proc: %v\n", err)
		os.Exit(1)
	}
	// 确保 /proc 可写，init 需写入 /proc/sys/vm/extra_free_kbytes 等
	_ = unix.Mount("", "/proc", "", unix.MS_REMOUNT, "rw")

	// 不在此处运行 procfs-simulator：Android init 启动后会重新挂载 /proc，会覆盖 FUSE。
	// 改为 fork 后由父进程在 init 启动后延迟执行 procfs-simulator。

	// If we have a new network namespace (container_network_isolated=true), bring up lo.
	// When using pod network (default), lo is already up.
	setupLoopback()

	// Snapshot pod IPv4 + gateway before /init/netd can flush Cilium-assigned addresses, then best-effort restore.
	saveCNIBootstrapSnapshot()
	restorePodNetwork()
	// AOSP Ethernet: STATIC + optional IPv6 InitialConfiguration (see patches/rk_aosp10).
	cloudphoneNetworkBootstrapBeforeInit()

	// Hide node-agent socket (and optional paths) from Android; cannot mask all of /shared/supervisord (binary lives there).
	maskPathsForRunContainerAndroid()

	// Setup cpuset so redroid init can write to /dev/cpuset/foreground/tasks.
	setupCpusetForRedroid()

	// Unmount /sys and /dev so Android init gets a clean slate on restart.
	// First run creates /dev/pts, mounts sysfs, etc. On stop+start, those persist and init fails with "File exists" / "Device or resource busy".
	unmountForInitRestart()

	// Mount sysfs (Android init needs it) and overlay simulated CPU data from cpu-simulator (if present).
	// cpu-simulator writes to /shared/cpu-sim; redroid will see simulated cpufreq, thermal.
	mountSysfsWithSimulatedCpu()

	// Fork helper：在 init 启动后执行延迟任务（cpuset、procfs-simulator、tombstones 等）
	// 然后 exec /init，让 init 继承 run_container 的 PID，成为 PID 1，正确回收僵尸
	helperArgv := append([]string{os.Args[0], runHelperMagic}, argv...)
	helperPid, err := syscall.ForkExec(os.Args[0], helperArgv, &syscall.ProcAttr{
		Dir:   "",
		Env:   os.Environ(),
		Files: []uintptr{0, 1, 2},
		Sys:   nil,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "run_container: fork helper: %v\n", err)
		os.Exit(1)
	}
	if helperPid == 0 {
		// helper 子进程：执行延迟任务后退出
		runHelperDelayedTasks()
		os.Exit(0)
	}
	// 父进程（run_container）：exec /init，让 init 继承当前 PID，成为 PID 1
	// exec 不会返回；若失败则 Exit
	if err := unix.Exec(target, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "run_container: exec %s: %v\n", target, err)
		os.Exit(1)
	}
	return true
}

// runHelperDelayedTasks 由 fork 出的 helper 进程执行，在 init 启动后完成 cpuset、procfs-simulator、tombstones 等延迟任务
func runHelperDelayedTasks() {
	mountCpusetAfterInitDevReady()
	time.Sleep(3 * time.Second)
	restorePodNetwork()
	// netd may flush eth0 long after boot; keep restoring in the background for the lifetime of this helper.
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for range tick.C {
			restorePodNetwork()
		}
	}()
	writeSystemResolvConfAfterBootCompleted()
	runProcfsSimulatorAfterInit()
	ensureTombstonesWritable()
	select {} // keep helper alive so periodic CNI restore keeps running
}

// mountCpusetAfterInitDevReady 在 init 创建 /dev 后 bind mount cpuset，供 init.rc copy 命令使用。
// init first stage 会重建 /dev，导致 setupCpusetForRedroid 的 /dev/cpuset 丢失；需在 init 创建 /dev 后重新挂载。
// 父进程与 init 共享 mount namespace（fork 继承），直接挂载即可。
// cgroup 文件系统只读，无法在其上创建 symlink；使用 overlay 在 upper 层添加 cpus/mems 指向 cpuset.cpus/mems。
func mountCpusetAfterInitDevReady() {
	// 轮询等待 init 创建 /dev（/dev/null 等），尽早 mount 赶在 init.rc copy 之前
	for i := 0; i < 60; i++ {
		if i > 0 {
			time.Sleep(50 * time.Millisecond)
		}
		if _, err := os.Stat("/dev/null"); err == nil {
			break
		}
	}
	cpusetRoot := findCpusetRoot()
	if cpusetRoot == "" {
		fmt.Fprintf(os.Stderr, "run_container: mountCpuset: findCpusetRoot empty\n")
		return
	}
	_ = os.MkdirAll("/dev/cpuset", 0755)
	_ = unix.Unmount("/dev/cpuset", unix.MNT_DETACH) // 若 init 创建了空目录，确保可挂载

	// cgroup 只读，无法直接创建 symlink。用 overlay：lower=cpuset，upper=可写层含 cpus->cpuset.cpus, mems->cpuset.mems
	// /tmp 可能是 overlay 根，不能作为 overlay upperdir。先挂载独立 tmpfs 作为 upper/work 所在文件系统。
	overlayBase := "/dev/.cpuset-overlay"
	_ = os.MkdirAll(overlayBase, 0755)
	_ = unix.Unmount(overlayBase, unix.MNT_DETACH)
	if err := unix.Mount("tmpfs", overlayBase, "tmpfs", 0, "size=4M"); err != nil {
		fmt.Fprintf(os.Stderr, "run_container: mount tmpfs for cpuset overlay: %v\n", err)
		if err := unix.Mount(cpusetRoot, "/dev/cpuset", "", unix.MS_BIND, ""); err != nil {
			fmt.Fprintf(os.Stderr, "run_container: bind mount cpuset: %v\n", err)
			return
		}
	} else {
		upper := filepath.Join(overlayBase, "upper")
		work := filepath.Join(overlayBase, "work")
		_ = os.MkdirAll(upper, 0755)
		_ = os.MkdirAll(work, 0755)
		// 用复制内容代替 symlink，避免 overlay 下 "Too many symbolic links"
		cpusData, _ := os.ReadFile(filepath.Join(cpusetRoot, "cpuset.cpus"))
		memsData, _ := os.ReadFile(filepath.Join(cpusetRoot, "cpuset.mems"))
		if len(cpusData) == 0 {
			cpusData = []byte("0")
		}
		if len(memsData) == 0 {
			memsData = []byte("0")
		}
		_ = os.WriteFile(filepath.Join(upper, "cpus"), cpusData, 0644)
		_ = os.WriteFile(filepath.Join(upper, "mems"), memsData, 0644)
		opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", cpusetRoot, upper, work)
		if err := unix.Mount("overlay", "/dev/cpuset", "overlay", 0, opts); err != nil {
			_ = unix.Unmount(overlayBase, unix.MNT_DETACH)
			if err := unix.Mount(cpusetRoot, "/dev/cpuset", "", unix.MS_BIND, ""); err != nil {
				fmt.Fprintf(os.Stderr, "run_container: mount cpuset: %v\n", err)
				return
			}
		}
	}

	// surfaceflinger 需要 /dev/cgroup_info/cgroup.rc。若 init 的 CgroupSetup 未执行（如 init 非 PID 1），
	// 预创建该文件供 libprocessgroup 读取。格式参考 Android libprocessgroup/setup/cgroup_map_write.cpp WriteRcFile。
	ensureCgroupRcForSurfaceflinger()
}

// ensureCgroupRcForSurfaceflinger 创建 /dev/cgroup_info/cgroup.rc，供 surfaceflinger 等通过 libprocessgroup 读取 cgroup 路径。
// Android 期望文件总长 64 字节：CgroupFile(8) + CgroupController(56)，其中 controller 为 version(4)+flags(4)+name(16)+path(32)
func ensureCgroupRcForSurfaceflinger() {
	const cgroupRcPath = "/dev/cgroup_info/cgroup.rc"
	if _, err := os.Stat(cgroupRcPath); err == nil {
		return // init 已创建，跳过
	}
	_ = os.MkdirAll("/dev/cgroup_info", 0711)
	// Android expects 64 bytes total (logcat: "expected 64, actual 112")
	const (
		fileVersion     = 1
		nameSize        = 16
		pathSize        = 32
		flagMounted     = 1
		cgroupVersionV1 = 1
	)
	controllers := []struct {
		name, path string
	}{
		{"cpuset", "/dev/cpuset"},
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(fileVersion))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(controllers)))
	for _, c := range controllers {
		_ = binary.Write(&buf, binary.LittleEndian, uint32(cgroupVersionV1))
		_ = binary.Write(&buf, binary.LittleEndian, uint32(flagMounted))
		name := make([]byte, nameSize)
		copy(name, c.name)
		buf.Write(name)
		path := make([]byte, pathSize)
		copy(path, c.path)
		buf.Write(path)
	}
	if err := os.WriteFile(cgroupRcPath, buf.Bytes(), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "run_container: write cgroup.rc: %v\n", err)
	}
}

// ensureTombstonesWritable 确保 /data/tombstones 对 tombstoned (uid 1058) 可写，消除 logcat 中
// "failed to link tombstone at trace_00: No such file or directory" 和 "missing output fd" 相关错误。
func ensureTombstonesWritable() {
	const tombstoneDir = "/data/tombstones"
	for i := 0; i < 60; i++ {
		time.Sleep(1 * time.Second)
		if st, err := os.Stat(tombstoneDir); err == nil && st.IsDir() {
			_ = os.Chmod(tombstoneDir, 0777) // tombstoned 需写入，容器环境放宽权限
			return
		}
	}
}

// runProcfsSimulatorAfterInit 在 init 启动后执行 procfs-simulator（init 会重新挂载 /proc，需在其之后替换）
// 通过读取 /dev/__properties__/u:object_r:boot_status_prop:s0 判断 sys.boot_completed 或 dev.bootcomplete 是否为 1
// 当 PROCFS_SIMULATOR_ENABLED 为 "true" 或 "1" 时启用，否则跳过（默认禁用，便于调试 scrcpy 黑屏、Watchdog 等）
func runProcfsSimulatorAfterInit() {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("PROCFS_SIMULATOR_ENABLED")))
	if v != "true" && v != "1" {
		return
	}
	const propPath = "/dev/__properties__/u:object_r:boot_status_prop:s0"
	const maxWait = 5 * time.Minute
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if isBootCompletedFromPropertyFile(propPath) {
			break
		}
	}
	if err := mountProcfs(); err != nil {
		fmt.Fprintf(os.Stderr, "run_container: procfs: %v\n", err)
	}
}

func setupLoopback() {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	_ = unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// Android cpuset cgroups that init writepid expects (see libprocessgroup/sched_policy.cpp).
var androidCpusetDirs = []string{"foreground", "system-background", "background", "top-app", "restricted", "camera-daemon"}

func setupCpusetForRedroid() {
	// Koordlet m+n wait runs in Supervisor.Reload(true) before any program (including container_run) starts.
	cpusetRoot := findCpusetRoot()
	if cpusetRoot == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(cpusetRoot, "cgroup.clone_children")); err == nil {
		_ = os.WriteFile(filepath.Join(cpusetRoot, "cgroup.clone_children"), []byte("1"), 0)
	}
	cpus, _ := os.ReadFile(filepath.Join(cpusetRoot, "cpuset.cpus"))
	mems, _ := os.ReadFile(filepath.Join(cpusetRoot, "cpuset.mems"))
	if len(cpus) == 0 || len(strings.TrimSpace(string(cpus))) == 0 {
		cpus = []byte("0")
		_ = os.WriteFile(filepath.Join(cpusetRoot, "cpuset.cpus"), cpus, 0)
	}
	if len(mems) == 0 || len(strings.TrimSpace(string(mems))) == 0 {
		mems = []byte("0")
		_ = os.WriteFile(filepath.Join(cpusetRoot, "cpuset.mems"), mems, 0)
	}
	// 创建 Android init 需要的 cpuset 子目录，writepid 会写入 tasks
	for _, name := range androidCpusetDirs {
		dir := filepath.Join(cpusetRoot, name)
		_ = os.MkdirAll(dir, 0755)
		_ = os.WriteFile(filepath.Join(dir, "cpuset.cpus"), cpus, 0)
		_ = os.WriteFile(filepath.Join(dir, "cpuset.mems"), mems, 0)
	}
	_ = os.MkdirAll("/dev/cpuset", 0755)
	_ = unix.Mount(cpusetRoot, "/dev/cpuset", "", unix.MS_BIND, "")
	// Android init.rc expects /dev/cpuset/cpus and /dev/cpuset/mems (copy command).
	// Cgroup v1 provides cpuset.cpus and cpuset.mems. Add symlinks for compatibility.
	_ = os.Symlink("cpuset.cpus", "/dev/cpuset/cpus")
	_ = os.Symlink("cpuset.mems", "/dev/cpuset/mems")
}

// unmountForInitRestart clears state so Android init gets a clean slate on restart.
// 不 unmount /sys（保留 /sys/fs/cgroup 供 init CgroupSetup 使用）。
// 不 unmount /dev/cpuset：若 unmount 留空，init 的 CgroupSetup 在容器内可能无法挂载（路径/权限），
// CgroupGetControllerPath 返回 false，出现 "cpuset cgroup controller is not mounted"。
// 保留 setupCpusetForRedroid 的 bind mount，init 可直接使用。
// Do NOT touch /dev - that would affect supervisord's namespace and break kubectl exec.
func unmountForInitRestart() {
	// 1. 保留 /dev/cpuset 的 bind mount（setupCpusetForRedroid 已挂载），init 的 writepid 可写入
	// 2. Unmount/remove /dev/__properties__ so Android init can recreate it on restart.
	_ = unix.Unmount("/dev/__properties__", unix.MNT_DETACH)
	_ = os.RemoveAll("/dev/__properties__")

	// 3. Restore pod IPv4/routes if netd flushed them (same as Cilium path).
	restorePodNetwork()
}

// writeSystemResolvConf copies /shared/etc/resolv.conf into /system/etc/resolv.conf.
// Best effort: do not block init startup when write fails.
func writeSystemResolvConf() {
	const (
		src = "/shared/etc/resolv.conf"
		dst = "/system/etc/resolv.conf"
	)
	data, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run_container: read %s: %v\n", src, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "run_container: mkdir %s: %v\n", filepath.Dir(dst), err)
		return
	}

	// Write directly to destination because overlay/bind mounts may reject rename.
	if err := os.WriteFile(dst, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "run_container: write %s: %v\n", dst, err)
		return
	}
}

// writeSystemResolvConfAfterBootCompleted waits until Android boot is completed
// and then writes /system/etc/resolv.conf.
func writeSystemResolvConfAfterBootCompleted() {
	const propPath = "/dev/__properties__/u:object_r:boot_status_prop:s0"
	const maxWait = 5 * time.Minute
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if isBootCompletedFromPropertyFile(propPath) {
			writeSystemResolvConf()
			return
		}
	}
	fmt.Fprintf(os.Stderr, "run_container: skip writing /system/etc/resolv.conf (boot_completed timeout)\n")
}

// mountSysfsWithSimulatedCpu overlays simulated CPU data from cpu-simulator onto /sys.
// cpu-simulator writes to /shared/cpu-sim; redroid will see simulated cpuinfo, cpufreq, thermal.
// 不重新挂载 sysfs，保留 /sys/fs/cgroup 供 init CgroupSetup 使用。
func mountSysfsWithSimulatedCpu() {
	simRoot := "/shared/cpu-sim"
	simCpu := filepath.Join(simRoot, "sys", "devices", "system", "cpu")
	if _, err := os.Stat(simCpu); err == nil {
		if err := unix.Mount(simCpu, "/sys/devices/system/cpu", "", unix.MS_BIND, ""); err != nil {
			fmt.Fprintf(os.Stderr, "run_container: bind mount cpu: %v\n", err)
		}
	}
	// Overlay simulated thermal
	simThermal := filepath.Join(simRoot, "sys", "class", "thermal")
	if _, err := os.Stat(simThermal); err == nil {
		if err := unix.Mount(simThermal, "/sys/class/thermal", "", unix.MS_BIND, ""); err != nil {
			fmt.Fprintf(os.Stderr, "run_container: bind mount thermal: %v\n", err)
		}
	}
}

func resolveIPBinary() string {
	for _, p := range []string{"/shared/toybox-bin/ip", "/shared/busybox/bin/ip", "/sbin/ip", "/usr/sbin/ip"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "ip"
}

func ipCmdEnv() []string {
	return append(os.Environ(), "PATH=/shared/bin:/shared/toybox-bin:/shared/busybox/bin:/sbin:/usr/sbin:/bin:/usr/bin")
}

// saveCNIBootstrapSnapshot writes /shared/cni-bootstrap/{ipv4,gateway}.txt from the current pod netns.
// Retries briefly: Cilium may program eth0 slightly after the main container process starts.
func saveCNIBootstrapSnapshot() {
	for attempt := 0; attempt < 20; attempt++ {
		if saveCNIBootstrapSnapshotOnce() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func saveCNIBootstrapSnapshotOnce() bool {
	ipbin := resolveIPBinary()
	out, err := exec.Command(ipbin, "-4", "-o", "addr", "show", "dev", "eth0").CombinedOutput()
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(out))
	fields := strings.Fields(line)
	var ipv4 string
	for i, f := range fields {
		if f == "inet" && i+1 < len(fields) {
			ipv4 = strings.Split(fields[i+1], "/")[0]
			break
		}
	}
	if ipv4 == "" || net.ParseIP(ipv4) == nil {
		return false
	}
	rtOut, err := exec.Command(ipbin, "-4", "route", "show", "default", "dev", "eth0").CombinedOutput()
	gw := ""
	if err == nil {
		fs := strings.Fields(strings.TrimSpace(string(rtOut)))
		for i, f := range fs {
			if f == "via" && i+1 < len(fs) {
				gw = fs[i+1]
				break
			}
		}
	}
	if gw == "" {
		rtOut2, err2 := exec.Command(ipbin, "-4", "route", "show", "default").CombinedOutput()
		if err2 == nil {
			fs := strings.Fields(strings.TrimSpace(string(rtOut2)))
			for i, f := range fs {
				if f == "via" && i+1 < len(fs) {
					gw = fs[i+1]
					break
				}
			}
		}
	}
	_ = os.MkdirAll(cniBootstrapDir, 0755)
	_ = os.WriteFile(filepath.Join(cniBootstrapDir, "ipv4.txt"), []byte(ipv4), 0644)
	if gw != "" && net.ParseIP(gw) != nil {
		_ = os.WriteFile(filepath.Join(cniBootstrapDir, "gateway.txt"), []byte(gw), 0644)
	}
	if ipv6, gw6 := detectIPv6Bootstrap(ipbin); ipv6 != "" {
		_ = os.WriteFile(filepath.Join(cniBootstrapDir, "ipv6.txt"), []byte(ipv6), 0644)
		if gw6 != "" {
			_ = os.WriteFile(filepath.Join(cniBootstrapDir, "gateway6.txt"), []byte(gw6), 0644)
		}
	}
	return true
}

func detectIPv6Bootstrap(ipbin string) (ipv6 string, gateway6 string) {
	out, err := exec.Command(ipbin, "-6", "-o", "addr", "show", "dev", "eth0", "scope", "global").CombinedOutput()
	if err == nil {
		line := strings.TrimSpace(string(out))
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "inet6" && i+1 < len(fields) {
				ipv6 = strings.Split(fields[i+1], "/")[0]
				break
			}
		}
	}
	if ip := net.ParseIP(ipv6); ip == nil || ip.To4() != nil {
		ipv6 = ""
	}
	rtOut, err := exec.Command(ipbin, "-6", "route", "show", "default", "dev", "eth0").CombinedOutput()
	if err == nil {
		fs := strings.Fields(strings.TrimSpace(string(rtOut)))
		for i, f := range fs {
			if f == "via" && i+1 < len(fs) {
				gateway6 = fs[i+1]
				break
			}
		}
	}
	if ip := net.ParseIP(gateway6); ip == nil || ip.To4() != nil {
		gateway6 = ""
	}
	return ipv6, gateway6
}

func guessCiliumGatewayFromPodIP(ipv4 string) string {
	ip := net.ParseIP(ipv4)
	if ip == nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	// Typical Cilium per-node next-hop: a.b.c.2 in the pod's /24-equivalent routing domain.
	return fmt.Sprintf("%d.%d.%d.2", v4[0], v4[1], v4[2])
}

// restorePodNetwork restores eth0 IPv4 + default route from bootstrap files, POD_IP env, or Calico legacy next-hop.
func restorePodNetwork() {
	ipbin := resolveIPBinary()
	env := ipCmdEnv()

	ipv4 := strings.TrimSpace(readFileTrim(filepath.Join(cniBootstrapDir, "ipv4.txt")))
	gw := strings.TrimSpace(readFileTrim(filepath.Join(cniBootstrapDir, "gateway.txt")))
	if ipv4 == "" {
		ipv4 = strings.TrimSpace(os.Getenv("POD_IP"))
	}
	if gw == "" && ipv4 != "" {
		gw = guessCiliumGatewayFromPodIP(ipv4)
	}

	if ipv4 != "" && net.ParseIP(ipv4) != nil {
		cmd := exec.Command(ipbin, "addr", "replace", ipv4+"/32", "dev", "eth0")
		cmd.Env = env
		_ = cmd.Run()
	}
	if gw != "" && net.ParseIP(gw) != nil {
		cmd := exec.Command(ipbin, "route", "replace", "default", "via", gw, "dev", "eth0")
		cmd.Env = env
		_ = cmd.Run()
		cmd2 := exec.Command(ipbin, "route", "replace", gw+"/32", "dev", "eth0", "scope", "link")
		cmd2.Env = env
		_ = cmd2.Run()
	} else if ipv4 == "" {
		// Legacy Calico (no snapshot / no POD_IP): next-hop 169.254.1.1
		restoreCalicoDefaultRoute()
	}

	// Restore IPv6 default route when available (Cilium dual-stack /128 pod IP case).
	// Android netd may remove it after init; keep it in both policy table and main.
	ipv6 := strings.TrimSpace(readFileTrim(filepath.Join(cniBootstrapDir, "ipv6.txt")))
	gw6 := strings.TrimSpace(readFileTrim(filepath.Join(cniBootstrapDir, "gateway6.txt")))
	if gw6 == "" {
		// Best-effort fallback for Cilium-style addressing: fd00::xxxx -> gateway fd00::xx0e.
		gw6 = guessCiliumGateway6FromPodIP(ipv6)
	}
	if ip := net.ParseIP(gw6); ip != nil && ip.To4() == nil {
		cmd := exec.Command(ipbin, "-6", "route", "replace", gw6+"/128", "dev", "eth0", "table", "eth0")
		cmd.Env = env
		_ = cmd.Run()
		cmd = exec.Command(ipbin, "-6", "route", "replace", "default", "via", gw6, "dev", "eth0", "table", "eth0")
		cmd.Env = env
		_ = cmd.Run()
		cmd = exec.Command(ipbin, "-6", "route", "replace", gw6+"/128", "dev", "eth0", "table", "main")
		cmd.Env = env
		_ = cmd.Run()
		cmd = exec.Command(ipbin, "-6", "route", "replace", "default", "via", gw6, "dev", "eth0", "table", "main")
		cmd.Env = env
		_ = cmd.Run()
	}
}

func guessCiliumGateway6FromPodIP(ipv6 string) string {
	ip := net.ParseIP(strings.TrimSpace(ipv6))
	if ip == nil || ip.To4() != nil {
		return ""
	}
	// Cilium node gateway convention in this cluster: fd00::<hi><lo> -> fd00::<hi>0e.
	// Example: pod fd00::3c48 -> node gateway fd00::3c0e.
	v6 := ip.To16()
	if v6 == nil {
		return ""
	}
	hi := v6[14]
	return fmt.Sprintf("fd00::%02x0e", hi)
}

func readFileTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// restoreCalicoDefaultRoute adds Calico's 169.254.1.1 default path; no-op on Cilium if routes already exist.
func restoreCalicoDefaultRoute() {
	path := resolveIPBinary()
	env := ipCmdEnv()
	cmd := exec.Command(path, "route", "add", "169.254.1.1", "dev", "eth0", "scope", "link")
	cmd.Env = env
	_ = cmd.Run()
	cmd = exec.Command(path, "route", "add", "default", "via", "169.254.1.1", "dev", "eth0")
	cmd.Env = env
	_ = cmd.Run()
}

// mountProcfs 用 FUSE procfs-simulator 替换 /proc
func mountProcfs() error {
	return mountProcfsSimulator()
}

// mountProcfsSimulator 用 FUSE procfs-simulator 替换 /proc
func mountProcfsSimulator() error {
	var bin string
	for _, p := range procfsSimulatorPaths {
		if _, err := os.Stat(p); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		return fmt.Errorf("procfs-simulator not found (tried %v)", procfsSimulatorPaths)
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		return fmt.Errorf("/dev/fuse not available (need fuse module): %w", err)
	}
	_ = os.MkdirAll("/proc.real", 0755)
	if err := unix.Mount("/proc", "/proc.real", "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind /proc to /proc.real: %w", err)
	}
	if err := unix.Unmount("/proc", unix.MNT_DETACH); err != nil {
		_ = unix.Unmount("/proc.real", unix.MNT_DETACH)
		return fmt.Errorf("umount /proc: %w", err)
	}
	args := []string{"-real=/proc.real", "-mount=/proc"}
	if os.Getenv("PROCFS_NO_FILTER_OVERLAY") == "1" || os.Getenv("PROCFS_NO_FILTER_OVERLAY") == "true" {
		args = append(args, "-no-filter-overlay")
	}
	if dev := os.Getenv("PROCFS_OVERLAY_ROOT_DEVICE"); dev != "" {
		if fstype := os.Getenv("PROCFS_OVERLAY_ROOT_FSTYPE"); fstype != "" {
			args = append(args, "-overlay-root-device="+dev, "-overlay-root-fstype="+fstype)
		}
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		restoreProc()
		return fmt.Errorf("start procfs-simulator: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		restoreProc()
		return fmt.Errorf("procfs-simulator exited: %w", err)
	case <-time.After(500 * time.Millisecond):
		if _, err := os.ReadFile("/proc/mounts"); err != nil {
			cmd.Process.Kill()
			<-done
			restoreProc()
			return fmt.Errorf("procfs-simulator mount not usable: %w", err)
		}
	}
	return nil
}

func restoreProc() {
	_ = unix.Unmount("/proc", unix.MNT_DETACH)
	_ = unix.Mount("/proc.real", "/proc", "", unix.MS_BIND, "")
	_ = unix.Unmount("/proc.real", unix.MNT_DETACH)
}

func findCpusetRoot() string {
	for _, root := range []string{"/sys/fs/cgroup/cpuset", "/sys/fs/cgroup"} {
		if _, err := os.Stat(filepath.Join(root, "cpuset.cpus")); err == nil {
			return root
		}
		if _, err := os.Stat(filepath.Join(root, "cpuset.cpus.effective")); err == nil {
			return root
		}
	}
	return ""
}
