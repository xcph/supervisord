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
	_, err := smolvmRunArgv("", 2, 2048, "/home/node/.openclaw", []string{"gateway-exec"})
	if err == nil || !strings.Contains(err.Error(), "CONTAINER_RUN_IMAGE") {
		t.Fatalf("image: %v", err)
	}
	args, err := smolvmRunArgv("registry.example/openclaw:latest", 2, 2048, "/home/node/.openclaw", []string{"/usr/local/bin/gateway-exec", "/usr/local/bin/node", "/app/openclaw.mjs", "gateway"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{
		"smolvm machine run",
		"--name openclaw-gateway",
		"--net-backend virtio-net",
		"--cpus 2",
		"--mem 2048",
		"-v /home/node/.openclaw:/home/node/.openclaw",
		"-p 127.0.0.1:18889:18889",
		"--image registry.example/openclaw:latest",
		"-- /usr/local/bin/gateway-exec",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	resume := strings.Join(smolvmResumeArgv(), " ")
	if resume != "smolvm machine resume --name openclaw-gateway" {
		t.Fatal(resume)
	}
}
