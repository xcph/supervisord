//go:build linux

package process

import (
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

func smolvmPreflight(image string) error {
	if strings.TrimSpace(image) == "" {
		return errImageRequired
	}
	if _, err := exec.LookPath("smolvm"); err != nil {
		if _, statErr := os.Stat("/usr/local/bin/smolvm"); statErr != nil {
			return errString("smolvm binary not found")
		}
	}
	f, err := os.Open("/dev/kvm")
	if err != nil {
		return errString("kvm unavailable")
	}
	_ = f.Close()
	return nil
}

func runADBRelay(listenAddr, target string, stop <-chan struct{}) {
	var ln net.Listener
	logged := false
	for {
		select {
		case <-stop:
			return
		default:
		}
		var err error
		ln, err = net.Listen("tcp", listenAddr)
		if err != nil {
			if !logged {
				logged = true
				log.WithFields(log.Fields{"addr": listenAddr, "err": err}).Warn("adb relay waiting for the smolvm address")
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			continue
		}
		break
	}
	defer ln.Close()
	go func() {
		<-stop
		_ = ln.Close()
	}()
	log.WithFields(log.Fields{"listen": listenAddr, "target": target}).Info("adb relay started")
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-stop:
				return
			default:
				continue
			}
		}
		go relayConn(conn, target)
	}
}

func relayConn(client net.Conn, target string) {
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
}
