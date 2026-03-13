# init 重启后网络不通 - 分析

## 架构回顾

- **container_network_isolated=false**（默认）：init 与 supervisord **共享** pod 的 network namespace
- init 有独立的：PID、mount、cgroup、UTS、IPC namespace
- 网络接口、路由、iptables 在共享的 net ns 中

## 可能原因

### 1. /dev/__properties__ 被清空导致网络属性丢失

`unmountForInitRestart` 会 `RemoveAll("/dev/__properties__")`。Android 的运行时属性（如 `dhcp.eth0.ipaddress`、`net.dns1`）存在这里。清空后：

- 新 init 启动时属性从零开始
- netd 可能依赖这些属性做网络配置
- DHCP 结果、DNS 等需要重新获取

**验证**：在 init 内执行 `getprop | grep -E 'dhcp|net\.|dns'` 对比首次启动与重启后。

### 2. iptables 规则冲突或丢失

- 首次 init 启动时 netd 会配置 iptables
- 重启后旧规则仍在，新 netd 可能重复添加或与旧规则冲突
- 或 netd 启动失败导致规则未正确应用

**验证**：主容器内 `iptables -L -n`、`iptables -t nat -L -n`，对比首次启动与重启后。

### 3. /sys 被 unmount 后 init 重新挂载不完整

`unmountForInitRestart` 会 unmount `/sys`。init 会重新挂载 sysfs，但：

- 若挂载顺序或时机异常，`/sys/class/net/` 等可能不可用
- netd 依赖 `/sys/class/net/` 获取接口信息

**验证**：在 init 内 `ls /sys/class/net/`，确认接口是否存在。

### 4. 网卡状态异常

- 首次 init 可能执行 `ip link set eth0 up` 等
- 重启后接口可能处于 down 或异常状态
- netd 可能未重新执行接口 up

**验证**：init 内 `ip link`、`ip addr` 查看接口状态。

### 5. DNS 配置

- `/etc/resolv.conf` 由 Kubernetes 注入
- init 的 mount ns 可能看不到正确的 resolv.conf
- 或 Android 使用自己的 DNS 配置，重启后未正确恢复

**验证**：init 内 `cat /etc/resolv.conf`，以及 `getprop net.dns1`。

## 诊断步骤

```bash
# 1. 进入 pod
kubectl exec my-cloudphone-pod -it -- sh

# 2. 运行诊断脚本（若已部署新镜像）
sh /shared/supervisord/diag-network-after-init-restart.sh

# 3. 进入 init namespace 测试网络
supervisord ctl exec init /shared/busybox/bin/sh -c 'ip addr; ip route; ping -c 1 8.8.8.8'

# 4. 查看 Android 网络相关属性
supervisord ctl exec init /system/bin/getprop 2>/dev/null | grep -E 'dhcp|net\.|dns' || true
```

## 临时绕过方案

若需快速恢复网络，可重启整个 Pod 而非只重启 init：

```bash
kubectl delete pod my-cloudphone-pod
# 等待新 Pod 拉起
```

## 潜在修复方向

1. **保留部分 /dev/__properties__**：不清空，或只清理 init 启动必需的部分，避免丢失网络相关属性
2. **重启前保存网络属性**：将 `dhcp.*`、`net.*` 等写入临时文件，init 启动后恢复
3. **netd 重启逻辑**：确认 redroid 的 netd 在 init 重启后能正确重新配置网络
4. **iptables 清理**：在 unmountForInitRestart 中或 init 启动前，选择性清理/重置 iptables（需谨慎，可能影响其他功能）
