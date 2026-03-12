package main

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/stun/v3"
	log "github.com/sirupsen/logrus"
)

// Default STUN servers for public IP discovery (try in order until one succeeds).
var defaultSTUNServers = []string{
	"stun:stun.qq.com:3478",
	"stun:stun.chat.bilibili.com:3478",
	"stun:stun.cloudflare.com:3478",
	"stun:stun.l.google.com:19302",
}

// getPublicIPFromSTUN queries STUN servers to discover the public IP and sets
// PUBLIC_IP environment variable. Non-blocking; on failure logs and continues.
// Child processes (e.g. /init) will inherit PUBLIC_IP via os.Environ().
func getPublicIPFromSTUN() {
	if os.Getenv("DISABLE_STUN") == "1" || os.Getenv("DISABLE_STUN") == "true" {
		return
	}
	servers := defaultSTUNServers
	if s := os.Getenv("STUN_SERVERS"); s != "" {
		servers = splitSTUNServers(s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, uri := range servers {
		ip, err := querySTUN(ctx, uri)
		if err != nil {
			log.WithFields(log.Fields{"server": uri, "error": err}).Debug("STUN query failed, trying next")
			continue
		}
		if ip != nil && len(ip) > 0 {
			addr := ip.String()
			os.Setenv("PUBLIC_IP", addr)
			log.WithFields(log.Fields{"public_ip": addr, "server": uri}).Info("discovered public IP via STUN")
			return
		}
	}
	log.Debug("could not discover public IP via STUN (all servers failed)")
}

func splitSTUNServers(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return defaultSTUNServers
	}
	return out
}

func querySTUN(ctx context.Context, uri string) (net.IP, error) {
	u, err := stun.ParseURI(uri)
	if err != nil {
		return nil, err
	}
	c, err := stun.DialURI(u, &stun.DialConfig{})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	var result net.IP
	var resultErr error
	var once sync.Once
	done := make(chan struct{})
	finish := func() { once.Do(func() { close(done) }) }
	go func() {
		err := c.Do(msg, func(res stun.Event) {
			if res.Error != nil {
				resultErr = res.Error
				finish()
				return
			}
			var xorAddr stun.XORMappedAddress
			if err := xorAddr.GetFrom(res.Message); err != nil {
				resultErr = err
				finish()
				return
			}
			result = xorAddr.IP
			finish()
		})
		if err != nil {
			resultErr = err
			finish()
		}
	}()
	select {
	case <-done:
		return result, resultErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
