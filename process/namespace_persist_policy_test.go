package process

import "testing"

func TestPersistNamespaceDecision(t *testing.T) {
	cases := []struct {
		name                   string
		containerRun, persist  bool
		runType, command, goos string
		want                   bool
	}{
		{"linux gateway", true, true, "namespace", "/usr/local/bin/gateway-exec", "linux", true},
		{"default off", true, false, "namespace", "sleep", "linux", false},
		{"container_run off", false, true, "namespace", "sleep", "linux", false},
		{"init ignored", true, true, "namespace", "/init", "linux", false},
		{"sbin init ignored", true, true, "namespace", "/sbin/init", "linux", false},
		{"smolvm ignored", true, true, "smolvm", "sleep", "linux", false},
		{"not linux", true, true, "namespace", "sleep", "darwin", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := persistNamespaceDecision(tc.containerRun, tc.persist, tc.runType, tc.command, tc.goos)
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
