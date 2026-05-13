package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gorilla/mux"
	"github.com/hashicorp/go-envparse"
	log "github.com/sirupsen/logrus"
)

// 与 docker/supervisord.d 中 envFilesOverlay 约定一致：仅允许直接子文件，避免路径穿越。
const runtimeEnvDir = "/etc/supervisord/env.d"

var validProgramNameForEnvAPI = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// putProgramEnvBody PUT /program/{name}/env
type putProgramEnvBody struct {
	Merge   *bool             `json:"merge"`
	Vars    map[string]string `json:"vars"`
	Remove  []string          `json:"remove"`
}

// patchProgramEnvBody PATCH /program/{name}/env — 单次修改一个键（合并进持久化文件）
type patchProgramEnvBody struct {
	Key    string  `json:"key"`
	Value  *string `json:"value,omitempty"`
	Delete bool    `json:"delete"`
}

type programEnvReply struct {
	Success         bool              `json:"success"`
	RestartRequired bool              `json:"restart_required"`
	Restarted       bool              `json:"restarted,omitempty"`
	Path            string            `json:"path,omitempty"`
	Message         string            `json:"message,omitempty"`
	Vars            map[string]string `json:"vars,omitempty"`
}

var putProgramReservedRootKeys = map[string]struct{}{
	"merge": {}, "vars": {}, "remove": {},
}

// parsePutProgramEnvBody unmarshals PUT /program/{name}/env JSON.
// Supports canonical shape {"merge":..., "vars":{...},"remove":[...]}
// plus flat overlays like {"FOO":"bar"} common with curl (-d '{"KEY":"..."}').
func parsePutProgramEnvBody(raw []byte) (putProgramEnvBody, error) {
	fields := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return putProgramEnvBody{Vars: map[string]string{}}, nil
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return putProgramEnvBody{}, err
	}
	var body putProgramEnvBody
	if rm, ok := fields["merge"]; ok && len(rm) > 0 && string(rm) != "null" {
		var merge bool
		if err := json.Unmarshal(rm, &merge); err != nil {
			return putProgramEnvBody{}, fmt.Errorf("merge: %w", err)
		}
		body.Merge = &merge
	}
	if rm, ok := fields["remove"]; ok && len(rm) > 0 && string(rm) != "null" {
		if err := json.Unmarshal(rm, &body.Remove); err != nil {
			return putProgramEnvBody{}, fmt.Errorf("remove: %w", err)
		}
	}
	fromVarsObj := map[string]string{}
	if vm, ok := fields["vars"]; ok && len(vm) > 0 && string(vm) != "null" {
		if err := json.Unmarshal(vm, &fromVarsObj); err != nil {
			return putProgramEnvBody{}, fmt.Errorf("vars: %w", err)
		}
	}
	body.Vars = make(map[string]string)
	for k, v := range fromVarsObj {
		body.Vars[k] = v
	}
	for k, rawVal := range fields {
		if _, skip := putProgramReservedRootKeys[k]; skip {
			continue
		}
		if !isValidEnvKey(k) {
			return putProgramEnvBody{}, fmt.Errorf("invalid env key %q", k)
		}
		var sv string
		if err := json.Unmarshal(rawVal, &sv); err != nil {
			return putProgramEnvBody{}, fmt.Errorf("key %s: expected string value: %w", k, err)
		}
		body.Vars[k] = sv
	}
	if body.Vars == nil {
		body.Vars = map[string]string{}
	}
	return body, nil
}

func (sr *SupervisorRestful) resolveRuntimeEnvPath(w http.ResponseWriter, program string) (string, bool) {
	if !validProgramNameForEnvAPI.MatchString(program) {
		http.Error(w, "invalid program name", http.StatusBadRequest)
		return "", false
	}
	entry := sr.supervisor.GetConfig().GetProgram(program)
	if entry == nil {
		http.Error(w, "unknown program", http.StatusNotFound)
		return "", false
	}
	overlay := strings.TrimSpace(entry.GetString("envFilesOverlay", ""))
	if overlay == "" {
		http.Error(w, "program has no envFilesOverlay; runtime env API disabled", http.StatusBadRequest)
		return "", false
	}
	clean := filepath.Clean(overlay)
	if filepath.Dir(clean) != runtimeEnvDir {
		http.Error(w, "envFilesOverlay must be a direct file under "+runtimeEnvDir, http.StatusForbidden)
		return "", false
	}
	if filepath.Base(clean) != program+".env" {
		http.Error(w, "envFilesOverlay must point to "+program+".env", http.StatusBadRequest)
		return "", false
	}
	return clean, true
}

func isValidEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func readEnvFileMap(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := envparse.Parse(f)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}

func encodeEnvValue(v string) string {
	if v == "" {
		return ""
	}
	if !strings.ContainsAny(v, " \t\n\"'#\\") {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func writeEnvFileAtomic(path string, vars map[string]string) error {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf strings.Builder
	for _, k := range keys {
		buf.WriteString(k)
		buf.WriteByte('=')
		buf.WriteString(encodeEnvValue(vars[k]))
		buf.WriteByte('\n')
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".env-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(buf.String()); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0644); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

func (sr *SupervisorRestful) respondAfterEnvWrite(w http.ResponseWriter, req *http.Request, name, path string, final map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	if err := writeEnvFileAtomic(path, final); err != nil {
		log.WithError(err).WithField("path", path).Error("write runtime env")
		http.Error(w, "write env file", http.StatusInternalServerError)
		return
	}
	reply := programEnvReply{
		Success:         true,
		RestartRequired: true,
		Path:            path,
		Message:         "环境已写入磁盘；子进程仅在 restart 后继承新变量（可传 ?restart=true）",
		Vars:            final,
	}
	wantRestart := req.URL.Query().Get("restart") == "1" || strings.EqualFold(req.URL.Query().Get("restart"), "true")
	wantImmediateRestart := immediateFromReq(req)
	if wantRestart {
		if _, err := sr._stopProgram(name, wantImmediateRestart); err != nil {
			reply.Message = "已写入 env，但 stop 失败: " + err.Error()
			_ = json.NewEncoder(w).Encode(reply)
			return
		}
		ok, err := sr._startProgram(name)
		reply.Restarted = ok && err == nil
		if !reply.Restarted {
			reply.Message = "已写入 env，但 restart 失败"
		} else {
			reply.Message = "已写入并重载进程"
			reply.RestartRequired = false
		}
	}
	_ = json.NewEncoder(w).Encode(reply)
}

// GetProgramEnv GET /program/{name}/env — 返回 envFilesOverlay 持久化文件中的键值。
func (sr *SupervisorRestful) GetProgramEnv(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	name := mux.Vars(req)["name"]
	path, ok := sr.resolveRuntimeEnvPath(w, name)
	if !ok {
		return
	}
	vars, err := readEnvFileMap(path)
	if err != nil {
		if os.IsNotExist(err) {
			_ = json.NewEncoder(w).Encode(programEnvReply{Success: true, Path: path, Vars: map[string]string{}})
			return
		}
		log.WithError(err).WithField("path", path).Error("read runtime env")
		http.Error(w, "read env file", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(programEnvReply{Success: true, Path: path, Vars: vars})
}

// PutProgramEnv PUT /program/{name}/env — 写入持久化 env；merge 省略时默认为 true。已运行进程需 restart（可选 query restart=true）；与 restart 同传 immediate=true 时先 SIGKILL 且不等待退出再拉起。
// JSON 接受：标准形 {"merge":true,"vars":{"K":"V"},"remove":[]}，以及常用简写顶层键 {"K":"V"}（与 vars 等价合并；扁平字段与 vars 同键时扁平覆盖）。
func (sr *SupervisorRestful) PutProgramEnv(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	defer req.Body.Close()
	name := mux.Vars(req)["name"]
	path, ok := sr.resolveRuntimeEnvPath(w, name)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	body, err := parsePutProgramEnvBody(raw)
	if err != nil {
		http.Error(w, "invalid json body: "+err.Error(), http.StatusBadRequest)
		return
	}
	for _, k := range body.Remove {
		if !isValidEnvKey(k) {
			http.Error(w, "invalid key in remove: "+k, http.StatusBadRequest)
			return
		}
	}
	for k := range body.Vars {
		if !isValidEnvKey(k) {
			http.Error(w, "invalid key in vars: "+k, http.StatusBadRequest)
			return
		}
	}

	merge := true
	if body.Merge != nil {
		merge = *body.Merge
	}

	final := make(map[string]string)
	if merge {
		existing, rerr := readEnvFileMap(path)
		if rerr != nil && !os.IsNotExist(rerr) {
			log.WithError(rerr).WithField("path", path).Error("read runtime env for merge")
			http.Error(w, "read env file", http.StatusInternalServerError)
			return
		}
		if existing != nil {
			for k, v := range existing {
				final[k] = v
			}
		}
	}
	for k, v := range body.Vars {
		final[k] = v
	}
	for _, k := range body.Remove {
		delete(final, k)
	}

	sr.respondAfterEnvWrite(w, req, name, path, final)
}

// PatchProgramEnv PATCH /program/{name}/env — 单键写入或删除（在既有持久化文件上合并）
func (sr *SupervisorRestful) PatchProgramEnv(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	defer req.Body.Close()
	name := mux.Vars(req)["name"]
	path, ok := sr.resolveRuntimeEnvPath(w, name)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var body patchProgramEnvBody
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if !isValidEnvKey(body.Key) {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}
	if body.Delete && body.Value != nil {
		http.Error(w, "delete and value are mutually exclusive", http.StatusBadRequest)
		return
	}
	if !body.Delete && body.Value == nil {
		http.Error(w, "require value or delete:true", http.StatusBadRequest)
		return
	}
	final := make(map[string]string)
	existing, rerr := readEnvFileMap(path)
	if rerr != nil && !os.IsNotExist(rerr) {
		log.WithError(rerr).WithField("path", path).Error("read runtime env for patch")
		http.Error(w, "read env file", http.StatusInternalServerError)
		return
	}
	if existing != nil {
		for k, v := range existing {
			final[k] = v
		}
	}
	if body.Delete {
		delete(final, body.Key)
	} else {
		final[body.Key] = *body.Value
	}
	sr.respondAfterEnvWrite(w, req, name, path, final)
}
