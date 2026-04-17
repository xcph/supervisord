package process

import "testing"

func TestMergeKeyValueArraysProgramOverridesOsHome(t *testing.T) {
	program := []string{"HOME=/home/node", "TERM=xterm"}
	system := []string{"HOME=/root", "PATH=/bin:/usr/bin", "LANG=C"}
	out := mergeKeyValueArrays(program, system)
	var home, path string
	for _, e := range out {
		switch {
		case len(e) >= 5 && e[:5] == "HOME=":
			home = e
		case len(e) >= 5 && e[:5] == "PATH=":
			path = e
		}
	}
	if home != "HOME=/home/node" {
		t.Fatalf("expected program HOME to override system, got %q in %#v", home, out)
	}
	if path != "PATH=/bin:/usr/bin" {
		t.Fatalf("expected PATH from system, got %q", path)
	}
}
