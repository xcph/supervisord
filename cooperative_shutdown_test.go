package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAdminListenToBaseURL(t *testing.T) {
	tests := map[string]string{
		"0.0.0.0:18890":   "http://127.0.0.1:18890",
		"127.0.0.1:18890": "http://127.0.0.1:18890",
		"":                "",
		"bad":             "",
	}
	for in, want := range tests {
		if got := adminListenToBaseURL(in); got != want {
			t.Fatalf("adminListenToBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLookupEnvMapOrOSSkipsPlaceholder(t *testing.T) {
	got := lookupEnvMapOrOS(map[string]string{
		"APP_ADMIN_TOKEN": "__REPLACE_WITH_ADMIN_TOKEN__",
	}, "APP_ADMIN_TOKEN")
	if got != "__REPLACE_WITH_ADMIN_TOKEN__" {
		t.Fatalf("lookup returns raw value for caller to filter, got %q", got)
	}
}

func TestReadStopHTTPRuntimeJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	body := []byte(`{"admin":{"listen":"0.0.0.0:18890","token":"secret"}}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := readStopHTTPRuntimeJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.Listen != "0.0.0.0:18890" || cfg.Admin.Token != "secret" {
		t.Fatalf("unexpected %#v", cfg)
	}
	if base := adminListenToBaseURL(cfg.Admin.Listen); base != "http://127.0.0.1:18890" {
		t.Fatalf("base=%q", base)
	}
}
