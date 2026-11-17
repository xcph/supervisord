package main

import (
	"testing"

	"github.com/ochinchina/supervisord/types"
)

func TestNamespaceExecPidPrefersKeeper(t *testing.T) {
	if got := namespaceExecPid(types.ProcessInfo{Pid: 1506, HostPid: 42}); got != 42 {
		t.Fatalf("pid=%d", got)
	}
	if got := namespaceExecPid(types.ProcessInfo{Pid: 7}); got != 7 {
		t.Fatalf("fallback pid=%d", got)
	}
	if got := namespaceExecPid(types.ProcessInfo{}); got != 0 {
		t.Fatalf("empty pid=%d", got)
	}
}
