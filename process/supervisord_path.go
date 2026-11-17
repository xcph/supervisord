package process

import (
	"os"
	"os/exec"
	"path/filepath"
)

func getSupervisordPath() string {
	if p := os.Args[0]; p != "" {
		if filepath.IsAbs(p) {
			return p
		}
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	for _, p := range []string{"/shared/supervisord/supervisord", "/usr/local/bin/supervisord"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("supervisord"); err == nil {
		return p
	}
	return "supervisord"
}
