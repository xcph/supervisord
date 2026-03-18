// procfs-simulator 使用 FUSE 实现虚拟 /proc：
// - mounts/mountinfo：根分区 overlay 替换为宿主机 overlayfs 底层块设备路径和文件系统类型
//
// 用法：
//   procfs-simulator -real=/proc.real -mount=/proc
//
// 需先执行：mount --bind /proc /proc.real && umount -l /proc
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

var (
	realPath              = flag.String("real", "/proc.real", "真实 /proc 挂载路径")
	mountPath             = flag.String("mount", "/proc", "FUSE 挂载点")
	debug                 = flag.Bool("debug", false, "启用 FUSE debug")
	noFilterOverlayFlag   = flag.Bool("no-filter-overlay", false, "不过滤 overlay 根分区（透传，解决 deviceinfo 等应用闪退）")
	overlayRootDeviceFlag = flag.String("overlay-root-device", "", "根分区 overlay 替换为指定设备路径（如 /dev/sda1），与 -overlay-root-fstype 同时设置时生效")
	overlayRootFstypeFlag = flag.String("overlay-root-fstype", "", "根分区 overlay 替换为指定文件系统类型（如 ext4），与 -overlay-root-device 同时设置时生效")
)

func main() {
	flag.Parse()

	if _, err := os.Stat(*realPath); os.IsNotExist(err) {
		log.Fatalf("real path %q does not exist (mount real proc there first)", *realPath)
	}

	root, err := newProcRoot(*realPath, *noFilterOverlayFlag, *overlayRootDeviceFlag, *overlayRootFstypeFlag)
	if err != nil {
		log.Fatalf("newProcRoot: %v", err)
	}

	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			AllowOther:  true,
			// 不启用 default_permissions：linker 的 readlink(/proc/self/fd/N) 会因内核权限检查失败 (Permission denied)
			// 透传 /proc 时由 FUSE 进程以 root 读取真实路径，无 default_permissions 可避免内核对调用者的额外检查
			// 仅传内核接受的选项；attr_timeout/entry_timeout 非 mount 选项，传之会导致 EINVAL
			// 仅传内核接受的选项；max_readahead 等部分选项在部分内核会导致 EINVAL
			Options:           []string{},
			DirectMount:       true,  // 直接调用 mount(2)
			DirectMountStrict: true,  // 不 fallback 到 fusermount，纯 Go 实现
		},
	}
	if *debug {
		opts.Debug = true
	}

	server, err := fs.Mount(*mountPath, root, opts)
	if err != nil {
		log.Fatalf("Mount: %v", err)
	}
	defer server.Unmount()

	fmt.Printf("procfs-simulator: serving %s (real: %s)\n", *mountPath, *realPath)
	server.Wait()
}
