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

func TestEnvSliceLookupPath(t *testing.T) {
	env := []string{"HOME=/home/node", "PATH=/home/node/.venv/bin:/usr/local/bin"}
	v, ok := envSliceLookup(env, "PATH")
	if !ok || v != "/home/node/.venv/bin:/usr/local/bin" {
		t.Fatalf("unexpected PATH lookup: ok=%v v=%q", ok, v)
	}
	_, ok2 := envSliceLookup(env, "MISSING")
	if ok2 {
		t.Fatalf("expected missing key ok=false")
	}
}

func TestEnvSliceLookupEmptyValue(t *testing.T) {
	v, ok := envSliceLookup([]string{"PATH="}, "PATH")
	if !ok || v != "" {
		t.Fatalf("expected PATH= empty, ok=%v v=%q", ok, v)
	}
}
