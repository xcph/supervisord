// +build linux

package process

import (
	"syscall"
)

func setDeathsig(sysProcAttr *syscall.SysProcAttr) {
	sysProcAttr.Setpgid = true
	// 不设 Pdeathsig：Go 运行时多线程 + PR_SET_PDEATHSIG(SIGKILL) 在部分内核/调度下会导致子进程
	// 在父进程仍存活时被误杀，表现为 openclaw-gateway 秒退、supervisord 报 Fatal。
}
