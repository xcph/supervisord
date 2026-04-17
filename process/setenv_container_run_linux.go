//go:build linux
// +build linux

package process

import (
	"strings"

	"github.com/ochinchina/supervisord/util"
	log "github.com/sirupsen/logrus"
)

// maybeInjectContainerRunAppArmorEnv sets internal env vars consumed by
// supervisord __run_container__ (see run_apparmor_linux.go) before the final exec.
func maybeInjectContainerRunAppArmorEnv(p *Process) {
	if p == nil || p.cmd == nil {
		return
	}
	if !p.config.GetBool("container_run", false) {
		return
	}
	log.WithFields(log.Fields{"program": p.GetName()}).Info("container_run enabled")
	prof := strings.TrimSpace(p.config.GetString("container_run_apparmor_profile", ""))
	if prof == "" {
		log.WithFields(log.Fields{"program": p.GetName()}).Info("container_run enabled but no container_run_apparmor_profile configured")
		return
	}
	if !util.AppArmorExecTransitionAvailable() {
		log.WithFields(log.Fields{"program": p.GetName()}).Info(
			"container_run_apparmor_profile / container_run_apparmor_relaxed ignored: AppArmor exec transition not available on this host",
		)
		return
	}
	p.cmd.Env = append(p.cmd.Env, "SUPERVISORD_APPARMOR_EXEC_PROFILE="+prof)
	log.WithFields(log.Fields{"program": p.GetName(), "profile": prof}).Info("container_run apparmor profile injected for __run_container__")
	if p.config.GetBool("container_run_apparmor_relaxed", false) {
		p.cmd.Env = append(p.cmd.Env, "SUPERVISORD_APPARMOR_RELAXED=1")
		log.WithFields(log.Fields{"program": p.GetName(), "profile": prof}).Info("container_run apparmor relaxed mode enabled")
	}
}
