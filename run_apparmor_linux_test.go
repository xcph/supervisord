//go:build linux
// +build linux

package main

import "testing"

func TestValidateAppArmorProfileName(t *testing.T) {
	if err := validateAppArmorProfileName(""); err == nil {
		t.Fatal("expected error")
	}
	if err := validateAppArmorProfileName("docker-default"); err != nil {
		t.Fatal(err)
	}
	if err := validateAppArmorProfileName("//mystack/profile"); err != nil {
		t.Fatal(err)
	}
	if err := validateAppArmorProfileName("bad\n"); err == nil {
		t.Fatal("expected error for newline")
	}
}

func TestFilterSupervisordInternalEnv(t *testing.T) {
	in := []string{
		"PATH=/bin",
		envAppArmorProfile + "=secretprof",
		"HOME=/root",
		envAppArmorRelaxed + "=1",
	}
	out := filterSupervisordInternalEnv(in)
	if len(out) != 2 {
		t.Fatalf("got %#v", out)
	}
}
