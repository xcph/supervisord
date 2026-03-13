#!/bin/sh
# 诊断 init 重启后网络不通的问题
# 用法: kubectl exec my-cloudphone-pod -it -- sh < scripts/diag-network-after-init-restart.sh
# 或: kubectl cp scripts/diag-network-after-init-restart.sh my-cloudphone-pod:/tmp/diag.sh && kubectl exec my-cloudphone-pod -it -- sh /tmp/diag.sh

echo "=== 1. 当前网络命名空间 (kubectl exec 进入的是主容器) ==="
readlink /proc/self/ns/net 2>/dev/null || echo "no /proc"

echo ""
echo "=== 2. 网卡状态 ==="
ip addr 2>/dev/null || ifconfig 2>/dev/null || cat /proc/net/dev 2>/dev/null

echo ""
echo "=== 3. 路由表 ==="
ip route 2>/dev/null || cat /proc/net/route 2>/dev/null

echo ""
echo "=== 4. init 进程 (supervisord 子进程，共享 net ns) ==="
# init 在 container_run 中，与 supervisord 共享 net ns (container_network_isolated=false)
ps aux 2>/dev/null | head -20 || ps 2>/dev/null

echo ""
echo "=== 5. iptables ==="
iptables -L -n 2>/dev/null | head -40 || echo "iptables not available"
iptables -t nat -L -n 2>/dev/null | head -20 || true

echo ""
echo "=== 6. DNS ==="
cat /etc/resolv.conf 2>/dev/null

echo ""
echo "=== 7. 主容器连通性 ==="
ping -c 1 8.8.8.8 2>/dev/null && echo "ping 8.8.8.8 OK" || echo "ping 8.8.8.8 FAIL"
ping -c 1 127.0.0.1 2>/dev/null && echo "ping 127.0.0.1 OK" || echo "ping 127.0.0.1 FAIL"

echo ""
echo "=== 8. init namespace 内连通性 (supervisord ctl exec init) ==="
echo "请手动执行: supervisord ctl exec init /system/bin/sh -c 'ping -c 1 8.8.8.8'"
echo "或: supervisord ctl exec init /shared/busybox/bin/sh -c 'ping -c 1 8.8.8.8'"

echo ""
echo "=== 9. /sys/class/net (主容器) ==="
ls -la /sys/class/net/ 2>/dev/null || echo "not found"
