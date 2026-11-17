//go:build !linux

package process

import (
	"io"
	"os"

	"github.com/ochinchina/supervisord/config"
)

// NamespaceKeeper is the Linux PID-1 that owns a persisted container namespace.
type NamespaceKeeper struct{}

func startNamespaceKeeper(string, io.Writer, io.Writer, *config.Entry) (*NamespaceKeeper, error) {
	return nil, os.ErrInvalid
}

func (k *NamespaceKeeper) spawn([]string, []string, string) (int, error) {
	return 0, os.ErrInvalid
}

func (k *NamespaceKeeper) signal(os.Signal) error { return os.ErrInvalid }

func (k *NamespaceKeeper) wait(int) (int, error) { return 0, os.ErrInvalid }

func (k *NamespaceKeeper) stop() {}

func (k *NamespaceKeeper) alive() bool { return false }

func MaybeRunNamespaceKeeper([]string) bool { return false }

func (p *Process) persistedChildAlive() bool { return false }

func (p *Process) NamespaceHostPid() int {
	if p == nil || p.namespaceKept.Load() {
		return 0
	}
	return p.cmdPid()
}
