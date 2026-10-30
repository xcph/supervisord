package process

import (
	"errors"
	"strings"
	"testing"
)

func TestChooseContainerRun(t *testing.T) {
	use, actual, reason := chooseContainerRun("namespace", false, nil)
	if use || actual != "namespace" || reason != "" {
		t.Fatalf("namespace: use=%v actual=%s reason=%s", use, actual, reason)
	}
	use, actual, reason = chooseContainerRun("smolvm", false, nil)
	if !use || actual != "smolvm" || reason != "" {
		t.Fatalf("smolvm: use=%v actual=%s reason=%s", use, actual, reason)
	}
	use, actual, reason = chooseContainerRun("smolvm", false, errors.New("kvm unavailable"))
	if use || actual != "namespace" || reason != "kvm unavailable" {
		t.Fatalf("preflight: use=%v actual=%s reason=%s", use, actual, reason)
	}
	use, actual, reason = chooseContainerRun("smolvm", true, nil)
	if use || actual != "namespace" || reason == "" {
		t.Fatalf("fallback: use=%v actual=%s reason=%s", use, actual, reason)
	}
}

func TestSmolvmRunArgv(t *testing.T) {
	_, err := smolvmRunArgv(containerRunSpec{machineName: "app"}, []string{"bin"})
	if err == nil || !strings.Contains(err.Error(), "CONTAINER_RUN_IMAGE") {
		t.Fatalf("image: %v", err)
	}
	args, err := smolvmRunArgv(containerRunSpec{
		image:       "registry.example/app:latest",
		cpus:        2,
		mem:         2048,
		stateDir:    "/var/lib/app",
		machineName: "my-app",
		publish:     "127.0.0.1:18889:18889",
	}, []string{"/usr/local/bin/app-exec", "/usr/local/bin/node", "main.mjs"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{
		"smolvm machine run",
		"--name my-app",
		"--net-backend virtio-net",
		"--cpus 2",
		"--mem 2048",
		"-v /var/lib/app:/var/lib/app",
		"-p 127.0.0.1:18889:18889",
		"--image registry.example/app:latest",
		"-- /usr/local/bin/app-exec",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	resume := strings.Join(smolvmResumeArgv("my-app"), " ")
	if resume != "smolvm machine resume --name my-app" {
		t.Fatal(resume)
	}
}

func TestSmolvmRunArgvOmitsEmptyStateDir(t *testing.T) {
	args, err := smolvmRunArgv(containerRunSpec{
		image:       "img:1",
		machineName: "x",
		publish:     "",
	}, []string{"cmd"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	if strings.Contains(got, "-v ") {
		t.Fatalf("unexpected volume mount: %s", got)
	}
	if strings.Contains(got, "-p ") {
		t.Fatalf("unexpected publish: %s", got)
	}
}
