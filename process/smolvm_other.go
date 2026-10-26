//go:build !linux

package process

func smolvmPreflight(image string) error {
	if image == "" {
		return errImageRequired
	}
	return errString("kvm unavailable")
}

func runADBRelay(_, _ string, stop <-chan struct{}) {
	<-stop
}
