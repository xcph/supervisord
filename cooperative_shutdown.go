package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ochinchina/supervisord/process"
	log "github.com/sirupsen/logrus"
)

const (
	openClawInitRuntimeConfigPath = "/tmp/openclaw-init.runtime.json"
	cooperativeShutdownTimeout    = 5 * time.Second
	cooperativeStopWaitTimeout    = 20 * time.Second
)

type openClawInitRuntimeConfig struct {
	Admin struct {
		Listen string `json:"listen"`
		Token  string `json:"token"`
	} `json:"admin"`
}

func resolveOpenClawInitAdminToken(envVars map[string]string) string {
	if t := strings.TrimSpace(envVars["OPENCLAW_INIT_ADMIN_TOKEN"]); t != "" && !strings.Contains(t, "__REPLACE") {
		return t
	}
	if t := strings.TrimSpace(os.Getenv("OPENCLAW_INIT_ADMIN_TOKEN")); t != "" && !strings.Contains(t, "__REPLACE") {
		return t
	}
	cfg, err := readOpenClawInitRuntimeConfig()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Admin.Token)
}

func resolveOpenClawInitAdminBaseURL(envVars map[string]string) string {
	if u := strings.TrimSpace(envVars["OPENCLAW_INIT_ADMIN_URL"]); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	if u := strings.TrimSpace(os.Getenv("OPENCLAW_INIT_ADMIN_URL")); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	cfg, err := readOpenClawInitRuntimeConfig()
	if err == nil {
		if u := adminListenToBaseURL(cfg.Admin.Listen); u != "" {
			return u
		}
	}
	return "http://127.0.0.1:18890"
}

func readOpenClawInitRuntimeConfig() (openClawInitRuntimeConfig, error) {
	var cfg openClawInitRuntimeConfig
	raw, err := os.ReadFile(openClawInitRuntimeConfigPath)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func adminListenToBaseURL(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return ""
	}
	host, port, ok := strings.Cut(listen, ":")
	if !ok {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "[::]" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}

// requestOpenClawInitCooperativeShutdown asks openclaw-init to exit via its admin API (self SIGTERM).
// Works when supervisord lacks CAP_KILL to signal user=node children (cap_drop ALL without CAP_KILL).
func requestOpenClawInitCooperativeShutdown(envVars map[string]string) error {
	token := resolveOpenClawInitAdminToken(envVars)
	if token == "" {
		return fmt.Errorf("openclaw-init admin token unavailable")
	}
	base := resolveOpenClawInitAdminBaseURL(envVars)
	req, err := http.NewRequest(http.MethodPost, base+"/v1/shutdown", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: cooperativeShutdownTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("admin shutdown HTTP %d", resp.StatusCode)
	}
	log.WithFields(log.Fields{"admin_base": base}).Info("openclaw-init cooperative shutdown requested")
	return nil
}

func (sr *SupervisorRestful) waitProgramStopped(name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proc := sr.supervisor.procMgr.Find(name)
		if proc == nil {
			return true
		}
		pid := proc.GetPid()
		state := proc.GetState()
		if pid == 0 && state != process.Running && state != process.Starting {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func (sr *SupervisorRestful) stopProgramForRestart(name string, immediate bool, envVars map[string]string) error {
	if immediate && name == "openclaw-init" {
		if err := requestOpenClawInitCooperativeShutdown(envVars); err != nil {
			log.WithError(err).Warn("openclaw-init cooperative shutdown failed; falling back to supervisord stop")
		} else if sr.waitProgramStopped(name, cooperativeStopWaitTimeout) {
			return nil
		} else {
			log.Warn("openclaw-init cooperative shutdown timed out; falling back to supervisord stop")
		}
	}
	_, err := sr._stopProgram(name, immediate)
	return err
}
