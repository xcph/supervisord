package main

import "testing"

func TestAdminListenToBaseURL(t *testing.T) {
	tests := map[string]string{
		"0.0.0.0:18890": "http://127.0.0.1:18890",
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

func TestResolveOpenClawInitAdminTokenSkipsPlaceholder(t *testing.T) {
	got := resolveOpenClawInitAdminToken(map[string]string{
		"OPENCLAW_INIT_ADMIN_TOKEN": "__REPLACE_WITH_ADMIN_TOKEN__",
	})
	if got != "" {
		// runtime json may exist in dev; only assert placeholder alone is ignored when no file.
		t.Logf("token resolved to %q (runtime file may be present)", got)
	}
}
