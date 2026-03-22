package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// 缓存过滤结果，降低 mounts/mountinfo 读取时的 CPU 占用
const (
	filterCacheTTL   = 5 * time.Second
	readCacheTTL     = 5 * time.Second // 提高 TTL 减少 dumpsys meminfo 等批量读时的 backend 压力
	readCacheMaxSize = 1024            // 扩大缓存以覆盖更多 /proc/<pid>/* 文件
)

var (
	filterCache struct {
		mu   sync.RWMutex
		data map[string]cacheEntry
	}
	readCache struct {
		mu   sync.RWMutex
		data map[string]cacheEntry
	}
)

type cacheEntry struct {
	data []byte
	ts   time.Time
}

var (
	noFilterOverlay      bool
	overlayRootDevice    string
	overlayRootFstype   string
)

func init() {
	filterCache.data = make(map[string]cacheEntry)
	readCache.data = make(map[string]cacheEntry)
}

func setNoFilterOverlay(v bool) { noFilterOverlay = v }

func setOverlayRootReplacement(device, fstype string) {
	overlayRootDevice = device
	overlayRootFstype = fstype
}

// 需要过滤 overlay 的路径（相对于 /proc）
var filterPaths = map[string]bool{
	"mounts":         true,
	"mountinfo":      true,
	"self/mounts":    true,
	"self/mountinfo": true,
	"1/mounts":       true,
	"1/mountinfo":    true,
}

// filterOverlayRootOnly 将根分区 overlay 替换为配置的 device/fstype（需 -overlay-root-device 与 -overlay-root-fstype）
func filterOverlayRootOnly(data []byte, procReal string) []byte {
	if noFilterOverlay {
		return data
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if !isRootMountLine(line) {
			continue
		}
		lines[i] = replaceRootMountLine(line, procReal)
	}
	return []byte(strings.Join(lines, "\n"))
}

// isRootMountLine 判断是否为根分区挂载行
func isRootMountLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return false
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return false
	}
	if parts[1] == "/" {
		return true
	}
	if len(parts) >= 5 && parts[4] == "/" {
		return true
	}
	return false
}

// 默认 overlay 替换：保持 fstype 为 overlay，仅替换 device 以隐藏容器路径。
// 反编译结论：deviceinfo 对 overlay 有特殊处理（不触发 getIdentifier），对 ext4/rootfs 等会调用 getIdentifier 且无对应 drawable 导致 Invalid ID。
// 不用 procfs-simulator 时应用读真实 /proc/mounts 看到 overlay 正常；用 procfs 时若替换为 ext4/rootfs 反而触发崩溃。故保持 fstype=overlay。
const (
	defaultOverlayDevice = "overlay"
	defaultOverlayFstype = "overlay"
)

// replaceRootMountLine 将根 overlay 行替换：隐藏 lowerdir/upperdir 等路径，但保持 fstype 为 overlay 以兼容 deviceinfo
func replaceRootMountLine(line string, _ string) string {
	if !strings.Contains(line, "overlay") {
		return line
	}
	device, fstype := overlayRootDevice, overlayRootFstype
	if device == "" {
		device = defaultOverlayDevice
	}
	if fstype == "" {
		fstype = defaultOverlayFstype
	}
	idx := strings.Index(line, " - ")
	if idx < 0 {
		// mounts 格式: device mount_point type options freq pass（用 rw 避免 overlay 的 lowerdir/upperdir 等混淆解析）
		parts := strings.Fields(line)
		if len(parts) >= 4 {
			return device + " / " + fstype + " rw 0 0"
		}
		return line
	}
	// mountinfo: before - type device super_options（super_options 用 rw 避免 overlay 特有选项混淆解析）
	before := line[:idx]
	after := line[idx+3:]
	afterParts := strings.Fields(after)
	if len(afterParts) >= 3 {
		parts := strings.Fields(before)
		if len(parts) >= 5 {
			opts := ""
			if len(parts) > 5 {
				opts = " " + strings.Join(parts[5:], " ")
			}
			return parts[0] + " " + parts[1] + " " + parts[2] + " / /" + opts + " - " + fstype + " " + device + " rw"
		}
	}
	return line
}

func isFilterPath(realPath, realRoot string) bool {
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if filterPaths[rel] {
		return true
	}
	parts := strings.Split(rel, "/")
	if len(parts) == 2 && (parts[1] == "mounts" || parts[1] == "mountinfo") {
		return true
	}
	return false
}

type procRoot struct {
	fs.Inode
	realPath string
	realRoot string
}

func newProcRoot(realPath string, noFilter bool, overlayDevice, overlayFstype string) (*procRoot, error) {
	setNoFilterOverlay(noFilter)
	setOverlayRootReplacement(overlayDevice, overlayFstype)
	return &procRoot{realPath: realPath, realRoot: realPath}, nil
}

func (r *procRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	// /proc/self 和 /proc/thread-self 必须解析为调用者 PID，否则 readlink(/proc/self/fd/N) 等会读到 FUSE 进程的 fd
	if caller, ok := fuse.FromContext(ctx); ok && (name == "self" || name == "thread-self") {
		target := strconv.Itoa(int(caller.Pid))
		child := &procSymlinkFixed{fs.Inode{}, target}
		return r.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFLNK}), 0
	}
	return lookupChild(ctx, r, filepath.Join(r.realPath, name), r.realRoot)
}

func (r *procRoot) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	return getattrNode(r.realPath, out)
}

func (r *procRoot) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	return readdirNode(r.realPath)
}

type procDir struct {
	fs.Inode
	realPath string
	realRoot string
}

func (d *procDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return lookupChild(ctx, d, filepath.Join(d.realPath, name), d.realRoot)
}

type nodeParent interface {
	NewInode(ctx context.Context, child fs.InodeEmbedder, attr fs.StableAttr) *fs.Inode
}

func lookupChild(ctx context.Context, parent nodeParent, realEntry, realRoot string) (*fs.Inode, syscall.Errno) {
	st, err := os.Lstat(realEntry)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	var child fs.InodeEmbedder
	var mode uint32
	switch {
	case st.Mode().IsDir():
		child = &procDir{fs.Inode{}, realEntry, realRoot}
		mode = fuse.S_IFDIR
	case st.Mode().IsRegular():
		child = &procFile{fs.Inode{}, realEntry, realRoot}
		mode = fuse.S_IFREG
	case st.Mode()&os.ModeSymlink != 0:
		child = &procSymlink{fs.Inode{}, realEntry, realRoot}
		mode = fuse.S_IFLNK
	default:
		child = &procFile{fs.Inode{}, realEntry, realRoot}
		mode = uint32(st.Mode())
	}
	return parent.NewInode(ctx, child, fs.StableAttr{Mode: mode}), 0
}

func (d *procDir) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	return getattrNode(d.realPath, out)
}

func (d *procDir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	return readdirNode(d.realPath)
}

type procFile struct {
	fs.Inode
	realPath string
	realRoot string
}

// procFileHandle 仅持路径，/proc 虚拟文件不能用 splice（会 invalid argument）
type procFileHandle struct {
	realPath string
	realRoot string
}

var _ fs.FileReader = (*procFileHandle)(nil)
var _ fs.FileWriter = (*procFileHandle)(nil)
var _ fs.FileReleaser = (*procFileHandle)(nil)

func (f *procFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	// FOPEN_DIRECT_IO：/proc 动态文件 GetAttr 报告 size=0，内核会据此限制读取量，
	// 导致 cat 收到空数据。非 DirectIO 时部分内核/场景下 crash_dump 等读 /proc/self/stat 会失败。
	return &procFileHandle{realPath: f.realPath, realRoot: f.realRoot}, uint32(fuse.FOPEN_DIRECT_IO), 0
}

func (h *procFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	base := filepath.Base(h.realPath)
	// 快速路径：仅 mounts/mountinfo 需要过滤，其余直接透传
	if base == "mounts" || base == "mountinfo" {
		if isFilterPath(h.realPath, h.realRoot) {
			data := getOrComputeFiltered(h.realPath, h.realRoot)
			if len(data) > 0 {
				return readResult(dest, data, off), 0
			}
		}
	}
	data, errno := getOrComputePassthrough(h.realPath)
	if errno != 0 {
		return nil, errno
	}
	return readResult(dest, data, off), 0
}

func readResult(dest, data []byte, off int64) fuse.ReadResult {
	end := off + int64(len(dest))
	if off >= int64(len(data)) {
		return fuse.ReadResultData(nil)
	}
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	n := copy(dest, data[off:end])
	return fuse.ReadResultData(dest[:n])
}

func getOrComputePassthrough(realPath string) ([]byte, syscall.Errno) {
	readCache.mu.RLock()
	if e, ok := readCache.data[realPath]; ok && time.Since(e.ts) < readCacheTTL {
		data := e.data
		readCache.mu.RUnlock()
		return data, 0
	}
	readCache.mu.RUnlock()

	data, err := os.ReadFile(realPath)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	readCache.mu.Lock()
	if len(readCache.data) >= readCacheMaxSize {
		for k := range readCache.data {
			delete(readCache.data, k)
			break
		}
	}
	readCache.data[realPath] = cacheEntry{data: data, ts: time.Now()}
	readCache.mu.Unlock()
	return data, 0
}

func getOrComputeFiltered(realPath, realRoot string) []byte {
	filterCache.mu.RLock()
	if e, ok := filterCache.data[realPath]; ok && time.Since(e.ts) < filterCacheTTL {
		data := e.data
		filterCache.mu.RUnlock()
		return data
	}
	filterCache.mu.RUnlock()

	filterCache.mu.Lock()
	defer filterCache.mu.Unlock()
	if e, ok := filterCache.data[realPath]; ok && time.Since(e.ts) < filterCacheTTL {
		return e.data
	}
	data, err := os.ReadFile(realPath)
	if err != nil {
		return nil
	}
	data = filterOverlayRootOnly(data, realRoot)
	// 限制 cache 大小，避免无限增长
	if len(filterCache.data) >= 32 {
		for k := range filterCache.data {
			delete(filterCache.data, k)
			break
		}
	}
	filterCache.data[realPath] = cacheEntry{data: data, ts: time.Now()}
	return data
}

func (h *procFileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	f, err := os.OpenFile(h.realPath, os.O_WRONLY, 0)
	if err != nil {
		return 0, fs.ToErrno(err)
	}
	defer f.Close()
	n, err := f.WriteAt(data, off)
	if err != nil {
		return 0, fs.ToErrno(err)
	}
	return uint32(n), 0
}

func (h *procFileHandle) Release(ctx context.Context) syscall.Errno {
	return 0
}

func (f *procFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	return getattrNode(f.realPath, out)
}

// procSymlinkFixed 返回固定 target 的符号链接，用于 /proc/self、/proc/thread-self
type procSymlinkFixed struct {
	fs.Inode
	target string
}

func (s *procSymlinkFixed) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	return []byte(s.target), 0
}

func (s *procSymlinkFixed) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFLNK | 0777
	out.Size = uint64(len(s.target))
	return 0
}

type procSymlink struct {
	fs.Inode
	realPath string
	realRoot string
}

func (s *procSymlink) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	target, err := os.Readlink(s.realPath)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	return []byte(target), 0
}

func (s *procSymlink) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	return getattrNode(s.realPath, out)
}

func getattrNode(path string, out *fuse.AttrOut) syscall.Errno {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return fs.ToErrno(err)
	}
	out.FromStat(&st)
	return 0
}

func readdirNode(path string) (fs.DirStream, syscall.Errno) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	var out []fuse.DirEntry
	for _, e := range entries {
		mode := fuse.S_IFREG
		if e.IsDir() {
			mode = fuse.S_IFDIR
		} else if e.Type()&os.ModeSymlink != 0 {
			mode = fuse.S_IFLNK
		}
		out = append(out, fuse.DirEntry{Name: e.Name(), Mode: uint32(mode)})
	}
	return fs.NewListDirStream(out), 0
}
