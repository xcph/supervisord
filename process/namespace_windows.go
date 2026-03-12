// +build windows

package process

import (
	"syscall"

	"github.com/ochinchina/supervisord/config"
)

func setContainerRun(_ *syscall.SysProcAttr, _ *config.Entry) {}

func createContainerRunWrapper(args []string) []string {
	return args
}
