package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ochinchina/supervisord/config"
	"github.com/ochinchina/supervisord/process"
	log "github.com/sirupsen/logrus"
)

const (
	defaultStopHTTPTimeoutSecs = 5
	defaultStopHTTPWaitSecs    = 20
	defaultStopHTTPPath        = "/v1/shutdown"
)

// stopHTTPRuntimeJSON is an optional discovery file for admin listen/token.
// Shape is product-agnostic: {"admin":{"listen":"host:port","token":"..."}}.
type stopHTTPRuntimeJSON struct {
	Admin struct {
		Listen string `json:"listen"`
		Token  string `json:"token"`
	} `json:"admin"`
}

type stopHTTPSpec struct {
	url        string
	token      string
	timeout    time.Duration
	wait       time.Duration
	enabled    bool
}

func (sr *SupervisorRestful) resolveStopHTTPSpec(name string, envVars map[string]string) stopHTTPSpec {
	spec := stopHTTPSpec{
		timeout: time.Duration(defaultStopHTTPTimeoutSecs) * time.Second,
		wait:    time.Duration(defaultStopHTTPWaitSecs) * time.Second,
	}
	proc := sr.supervisor.procMgr.Find(name)
	var entry *config.Entry
	if proc != nil {
		entry = proc.GetConfig()
	}
	if entry == nil {
		return spec
	}

	timeoutSecs := entry.GetInt("stop_http_timeout_secs", defaultStopHTTPTimeoutSecs)
	if timeoutSecs > 0 {
		spec.timeout = time.Duration(timeoutSecs) * time.Second
	}
	waitSecs := entry.GetInt("stop_http_wait_secs", defaultStopHTTPWaitSecs)
	if waitSecs > 0 {
		spec.wait = time.Duration(waitSecs) * time.Second
	}

	url := strings.TrimSpace(entry.GetString("stop_http_url", ""))
	path := strings.TrimSpace(entry.GetString("stop_http_path", defaultStopHTTPPath))
	if path == "" {
		path = defaultStopHTTPPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	runtimePath := strings.TrimSpace(entry.GetString("stop_http_runtime_json", ""))
	var runtime stopHTTPRuntimeJSON
	hasRuntime := false
	if runtimePath != "" {
		if cfg, err := readStopHTTPRuntimeJSON(runtimePath); err == nil {
			runtime = cfg
			hasRuntime = true
		}
	}

	if url == "" && hasRuntime {
		if base := adminListenToBaseURL(runtime.Admin.Listen); base != "" {
			url = base + path
		}
	}
	if url == "" {
		return spec
	}
	spec.url = url
	spec.enabled = true

	token := strings.TrimSpace(entry.GetString("stop_http_token", ""))
	tokenEnv := strings.TrimSpace(entry.GetString("stop_http_token_env", ""))
	if token == "" || strings.Contains(token, "__REPLACE") {
		token = ""
	}
	if token == "" && tokenEnv != "" {
		token = lookupStopHTTPToken(entry, envVars, tokenEnv)
	}
	if (token == "" || strings.Contains(token, "__REPLACE")) && hasRuntime {
		if t := strings.TrimSpace(runtime.Admin.Token); t != "" && !strings.Contains(t, "__REPLACE") {
			token = t
		}
	}
	if strings.Contains(token, "__REPLACE") {
		token = ""
	}
	spec.token = token
	return spec
}

func lookupEnvMapOrOS(envVars map[string]string, key string) string {
	if envVars != nil {
		if v := strings.TrimSpace(envVars[key]); v != "" {
			return v
		}
	}
	return strings.TrimSpace(os.Getenv(key))
}

func lookupStopHTTPToken(entry *config.Entry, envVars map[string]string, tokenEnv string) string {
	if t := lookupEnvMapOrOS(envVars, tokenEnv); t != "" {
		return t
	}
	prefix := tokenEnv + "="
	for _, item := range entry.GetEnvFromFiles("envFilesOverlay") {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimSpace(item[len(prefix):])
		}
	}
	for _, item := range entry.GetEnv("environment") {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimSpace(item[len(prefix):])
		}
	}
	return ""
}

func readStopHTTPRuntimeJSON(path string) (stopHTTPRuntimeJSON, error) {
	var cfg stopHTTPRuntimeJSON
	raw, err := os.ReadFile(path)
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

// requestCooperativeHTTPShutdown asks the program to exit via a configured HTTP endpoint
// (useful when supervisord lacks CAP_KILL to signal dropped-privilege children).
func requestCooperativeHTTPShutdown(spec stopHTTPSpec) error {
	if !spec.enabled || spec.url == "" {
		return fmt.Errorf("stop_http_url not configured")
	}
	if spec.token == "" {
		return fmt.Errorf("stop_http token unavailable")
	}
	req, err := http.NewRequest(http.MethodPost, spec.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+spec.token)
	client := &http.Client{Timeout: spec.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stop_http HTTP %d", resp.StatusCode)
	}
	log.WithFields(log.Fields{"url": spec.url}).Info("cooperative HTTP shutdown requested")
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
	if immediate {
		spec := sr.resolveStopHTTPSpec(name, envVars)
		if spec.enabled {
			if err := requestCooperativeHTTPShutdown(spec); err != nil {
				log.WithError(err).WithField("program", name).Warn("cooperative HTTP shutdown failed; falling back to supervisord stop")
			} else if sr.waitProgramStopped(name, spec.wait) {
				return nil
			} else {
				log.WithField("program", name).Warn("cooperative HTTP shutdown timed out; falling back to supervisord stop")
			}
		}
	}
	_, err := sr._stopProgram(name, immediate)
	return err
}
