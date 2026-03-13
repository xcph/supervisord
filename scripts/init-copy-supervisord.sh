#!/bin/sh
# Init container script: copy supervisord binary and config to /shared/supervisord/.
# The main container runs /shared/supervisord/supervisord.
set -e

SHARED="${SHARED_DIR:-/shared}"
mkdir -p "$SHARED"

# Copy supervisord binary and config to /shared/supervisord/
mkdir -p "$SHARED/supervisord" "$SHARED/bin"
cp -f /usr/local/bin/supervisord "$SHARED/supervisord/supervisord"
cp -f /usr/local/bin/exec-in-ns "$SHARED/supervisord/exec-in-ns"
[ -f /usr/local/bin/curl ] && cp -f /usr/local/bin/curl "$SHARED/bin/curl" && chmod +x "$SHARED/bin/curl"
[ -f /scripts/diag-network-after-init-restart.sh ] && cp -f /scripts/diag-network-after-init-restart.sh "$SHARED/supervisord/"
chmod +x "$SHARED/supervisord/supervisord" "$SHARED/supervisord/exec-in-ns"
# Symlink so "supervisord" is found in PATH for ctl exec
ln -sf "$SHARED/supervisord/supervisord" "$SHARED/bin/supervisord"

# Copy toybox and busybox to /shared (for kubectl exec; redroid has no /bin/sh)
HAS_UTILS=0
SH_SHELL="/shared/busybox/bin/sh"
if [ -d /usr/local/toybox-bin ]; then
  mkdir -p "$SHARED/toybox-bin"
  cp -a /usr/local/toybox-bin/. "$SHARED/toybox-bin/"
  chmod -R a+x "$SHARED/toybox-bin"
  HAS_UTILS=1
  [ -d /usr/local/busybox ] || SH_SHELL="/shared/toybox-bin/sh"
fi
if [ -d /usr/local/busybox ]; then
  mkdir -p "$SHARED/busybox"
  cp -a /usr/local/busybox/. "$SHARED/busybox/"
  chmod -R a+x "$SHARED/busybox"
  # Ensure nc and telnet symlinks exist (busybox applets)
  for cmd in nc netcat telnet; do
    [ ! -e "$SHARED/busybox/bin/$cmd" ] && ln -sf busybox "$SHARED/busybox/bin/$cmd"
  done
  HAS_UTILS=1
  SH_SHELL="/shared/busybox/bin/sh"
fi

# Create passwd/group for root
mkdir -p "$SHARED/etc"
printf '%s\n' "root:x:0:0:root:/root:$SH_SHELL" > "$SHARED/etc/passwd"
printf '%s\n' 'root:x:0:' > "$SHARED/etc/group"
sync

# Write supervisord.conf
# - nodaemon: run in foreground for container
# - program:init with container_run: run /init in isolated container (runc-like)
# - INIT_COMMAND: default /init, overridable via env
# - INIT_ARGS: optional args for /init (space-separated, from env)
# - CONTAINER_RUN: "true" runs /init as PID 1 in new ns (required for redroid). "false" = /init as child, netd fails.
# - CONTAINER_NETWORK_ISOLATED: "true" adds CLONE_NEWNET; default false (redroid needs pod net for iptables)
INIT_CMD="${INIT_COMMAND:-/init}"
CONTAINER_RUN="${CONTAINER_RUN:-true}"
CONTAINER_NETWORK_ISOLATED="${CONTAINER_NETWORK_ISOLATED:-false}"
{
  echo '[supervisord]'
  echo 'nodaemon=true'
  echo 'logfile=/dev/stdout'
  echo 'logfile_maxbytes=0'
  echo ''
  echo '[inet_http_server]'
  echo 'port=127.0.0.1:9001'
  echo ''
  echo '[supervisorctl]'
  echo 'serverurl=http://127.0.0.1:9001'
  echo ''
  if [ "$HAS_UTILS" = 1 ]; then
    echo '# Ensure /etc/passwd, /etc/group, /root (fix su/id; su needs root home dir)'
    echo '[program:setup-etc]'
    echo "command=$SH_SHELL -c \"mkdir -p /etc /root; echo \\\"root:x:0:0:root:/root:$SH_SHELL\\\" > /etc/passwd; echo \\\"root:x:0:\\\" > /etc/group\""
    echo 'autostart=true'
    echo 'autorestart=false'
    echo 'startsecs=0'
    echo 'priority=1'
    echo 'stdout_logfile=/dev/stdout'
    echo 'stdout_logfile_maxbytes=0'
    echo 'stderr_logfile=/dev/stderr'
    echo 'stderr_logfile_maxbytes=0'
    echo ''
    echo '[program-default]'
    echo 'environment=PATH="/shared/bin:/shared/supervisord:/shared/busybox/bin:/shared/toybox-bin:/bin:/usr/bin:/system/bin",SUPERVISORD_PATH="/shared/supervisord/supervisord",EXEC_IN_NS_PATH="/shared/supervisord/exec-in-ns"'
    echo ''
  fi
  echo '[program:init]'
  if [ -n "$INIT_ARGS" ]; then
    echo "command=$INIT_CMD $INIT_ARGS"
  else
    echo "command=$INIT_CMD"
  fi
  echo 'autostart=true'
  echo 'autorestart=true'
  echo 'priority=2'
  echo 'stdout_logfile=/dev/stdout'
  echo 'stdout_logfile_maxbytes=0'
  echo 'stderr_logfile=/dev/stderr'
  echo 'stderr_logfile_maxbytes=0'
  echo "container_run=$CONTAINER_RUN"
  echo "container_network_isolated=$CONTAINER_NETWORK_ISOLATED"
} > "$SHARED/supervisord/supervisord.conf"

echo "supervisord and config copied to $SHARED/supervisord"
