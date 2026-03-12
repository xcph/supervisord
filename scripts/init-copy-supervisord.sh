#!/bin/sh
# Init container script: copy supervisord binary and write config to /shared volume.
# The main container will run supervisord from /shared/supervisord.
set -e

SHARED="${SHARED_DIR:-/shared}"
mkdir -p "$SHARED"

# Copy supervisord binary
cp -f /usr/local/bin/supervisord "$SHARED/supervisord"
chmod +x "$SHARED/supervisord"

# Copy static busybox for setup-shell (redroid has no /bin/sh; kubectl exec -- sh needs it)
HAS_BUSYBOX=0
for bb in /bin/busybox /usr/bin/busybox; do
  if [ -f "$bb" ]; then
    cp -f "$bb" "$SHARED/busybox"
    chmod +x "$SHARED/busybox"
    HAS_BUSYBOX=1
    break
  fi
done

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
  if [ "$HAS_BUSYBOX" = 1 ]; then
    echo '# Create /bin/sh symlink so kubectl exec -- sh works (redroid has no /bin/sh)'
    echo '[program:setup-shell]'
    echo 'command=/shared/busybox sh -c "mkdir -p /bin && ln -sf /system/bin/sh /bin/sh"'
    echo 'autostart=true'
    echo 'autorestart=false'
    echo 'startsecs=0'
    echo 'priority=1'
    echo 'stdout_logfile=/dev/stdout'
    echo 'stdout_logfile_maxbytes=0'
    echo 'stderr_logfile=/dev/stderr'
    echo 'stderr_logfile_maxbytes=0'
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
} > "$SHARED/supervisord.conf"

echo "supervisord and config copied to $SHARED"
