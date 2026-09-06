package srt

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
)

// fakeProxy answers one CONNECT: with the given status and, on 200, the
// probe header. It records the Proxy-Authorization it saw.
func fakeProxy(t *testing.T, status int, nonce string) (addr string, seenAuth *string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var auth string
	seenAuth = &auth
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		req, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			return
		}
		auth = req.Header.Get("Proxy-Authorization")
		if status == 200 {
			fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n%s: %s\r\n\r\n", probeHeader, nonce)
			return
		}
		fmt.Fprintf(c, "HTTP/1.1 %d Forbidden\r\nX-Proxy-Error: blocked-by-allowlist\r\nContent-Length: 0\r\n\r\n", status)
	}()
	return "http://" + ln.Addr().String(), seenAuth
}

// TestProbeRein: the probe accepts only a 200 carrying THIS run's nonce, sends
// the secret as srt's basic auth, and names srt's own refusal when srt (not
// rein) answers.
func TestProbeRein(t *testing.T) {
	addr, auth := fakeProxy(t, 200, "n1")
	if err := probeRein(addr, "sec", "n1"); err != nil {
		t.Errorf("matching nonce: %v", err)
	}
	if *auth == "" || !strings.HasPrefix(*auth, "Basic ") {
		t.Errorf("probe sent no basic proxy auth: %q", *auth)
	}
	addr, _ = fakeProxy(t, 200, "other")
	if err := probeRein(addr, "sec", "n1"); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Errorf("foreign nonce accepted: %v", err)
	}
	addr, _ = fakeProxy(t, 403, "")
	if err := probeRein(addr, "sec", "n1"); err == nil || !strings.Contains(err.Error(), "blocked-by-allowlist") {
		t.Errorf("srt-style 403 not surfaced: %v", err)
	}
	if err := probeRein("nonsense", "sec", "n1"); err == nil {
		t.Error("bad proxy url accepted")
	}
}
