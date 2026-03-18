//go:build linux
// +build linux

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
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
const runHelperMagic   = "__run_helper__"

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

	// 确保 Calico 默认路由存在。Calico CNI 在容器启动时添加，但 Android init/netd 可能清除策略路由导致 default 丢失。
	restoreCalicoDefaultRoute()

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
	restoreCalicoDefaultRoute()
	runProcfsSimulatorAfterInit()
	ensureTombstonesWritable()
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

	// 3. Restore Calico default route. When init/netd dies, it may flush policy routing; Calico uses 169.254.1.1.
	restoreCalicoDefaultRoute()
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

func restoreCalicoDefaultRoute() {
	path := "ip"
	for _, p := range []string{"/shared/toybox-bin/ip", "/shared/busybox/bin/ip", "/sbin/ip", "/usr/sbin/ip"} {
		if _, err := os.Stat(p); err == nil {
			path = p
			break
		}
	}
	env := append(os.Environ(), "PATH=/shared/bin:/shared/toybox-bin:/shared/busybox/bin:/sbin:/usr/sbin:/bin:/usr/bin")
	cmd := exec.Command(path, "route", "add", "169.254.1.1", "dev", "eth0", "scope", "link")
	cmd.Env = env
	_ = cmd.Run() // ignore error (route may exist)
	cmd = exec.Command(path, "route", "add", "default", "via", "169.254.1.1", "dev", "eth0")
	cmd.Env = env
	_ = cmd.Run() // ignore error (route may exist)
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
