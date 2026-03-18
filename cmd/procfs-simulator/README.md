# procfs-simulator

使用 FUSE 实现虚拟 /proc，在 `mounts`、`mountinfo` 的 read 中返回过滤后的内容（overlay/overlayfs → ext4），`meminfo` 虚拟化为 cgroup 限制，其余路径透传到真实 /proc。

---

## 一、编译

### 方式 1：Docker 多架构构建（推荐）

在任意主机（含 Mac/Windows）上构建，纯 Go 无需 libfuse：

```bash
cd supervisord/cmd/procfs-simulator

# 构建当前架构
./build.sh

# 指定架构
./build.sh amd64
./build.sh arm64
```

输出：`./procfs-simulator`、`./hook-fuse.sh`

### 方式 2：本地构建（含交叉编译）

纯 Go 实现，CGO_ENABLED=0 可静态编译，无需 libfuse：

```bash
cd supervisord/cmd/procfs-simulator

# 当前架构
go build -o procfs-simulator .

# 交叉编译（如 Mac 上构建 Linux arm64）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o procfs-simulator .
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o procfs-simulator .
```

### 方式 3：Docker 镜像构建

```bash
docker build -t procfs-simulator:latest .
# 从镜像中提取二进制
docker create --name tmp procfs-simulator
docker cp tmp:/usr/local/bin/procfs-simulator .
docker cp tmp:/usr/local/bin/hook-fuse.sh .
docker rm tmp
```

---

## 二、部署

### 1. 节点依赖

```bash
# Debian/Ubuntu
apt-get install -y fuse3 libfuse3-3 jq

# RHEL/CentOS
yum install -y fuse3 jq

# 确保 FUSE 模块已加载
modprobe fuse
```

### 2. 安装文件

```bash
# 复制到节点
cp procfs-simulator /usr/local/bin/
cp hook-fuse.sh /usr/local/bin/
cp runc-wrapper-fuse.sh /usr/local/bin/hide-overlayfs-runc
chmod +x /usr/local/bin/procfs-simulator \
        /usr/local/bin/hook-fuse.sh \
        /usr/local/bin/hide-overlayfs-runc
```

或使用 Makefile：

```bash
make install
# 需手动复制 runc-wrapper-fuse.sh 为 hide-overlayfs-runc
```

### 3. 配置 containerd

编辑 `/etc/containerd/config.toml`：

```toml
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
    BinaryName = "/usr/local/bin/hide-overlayfs-runc"
```

或新增自定义 runtime：

```toml
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.hide-overlayfs]
  runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.hide-overlayfs.options]
    BinaryName = "/usr/local/bin/hide-overlayfs-runc"
```

重启 containerd：

```bash
systemctl restart containerd
```

### 4. 配置 Docker

编辑 `/etc/docker/daemon.json`：

```json
{
  "runtimes": {
    "hide-overlayfs": {
      "path": "/usr/local/bin/hide-overlayfs-runc"
    }
  },
  "default-runtime": "hide-overlayfs"
}
```

或仅对指定容器使用：`docker run --runtime=hide-overlayfs ...`

重启 Docker：

```bash
systemctl restart docker
```

---

## 三、验证

```bash
# 容器内执行
docker run --rm alpine cat /proc/mounts | head -5
# 或
kubectl exec -it <pod> -- cat /proc/mounts | head -5
```

应看到 `ext4` 而非 `overlay`/`overlayfs`。

---

## 四、手动测试（非容器）

```bash
# 1. 备份真实 proc
sudo mkdir -p /proc.real
sudo mount --bind /proc /proc.real

# 2. 卸载 /proc
sudo umount -l /proc

# 3. 启动 FUSE（前台运行）
sudo ./procfs-simulator -real=/proc.real -mount=/proc

# 4. 另开终端验证
cat /proc/mounts | head -5
```

---

## 五、架构

```
容器内 /proc  ← FUSE 挂载（过滤 mounts/mountinfo）
       │
       └── passthrough → /proc.real（真实 proc）
```

## 六、过滤路径

**Overlay 替换**（根分区 overlay → ext4）：
- `/proc/mounts`、`/proc/mountinfo`
- `/proc/self/mounts`、`/proc/self/mountinfo`
- `/proc/1/mounts`、`/proc/1/mountinfo`

**内存虚拟化**（`free` 显示 cgroup limit 而非宿主机内存）：
- `/proc/meminfo`、`/proc/self/meminfo`、`/proc/1/meminfo`

## 七、依赖

**构建**：Go 1.21+，纯 Go（CGO_ENABLED=0 可静态编译）  
**运行**：fuse3、libfuse3-3、jq  
**内核**：FUSE 模块  
**权限**：root 或 `/etc/fuse.conf` 中 `user_allow_other`

---

## 八、快速参考

```bash
# 编译 + 部署（完整流程）
cd supervisord/cmd/procfs-simulator
./build.sh amd64                    # 或 arm64
sudo make install                   # 安装到 /usr/local/bin
# 配置 containerd/docker 后重启
sudo systemctl restart containerd   # 或 docker
```
