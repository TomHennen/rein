package runbroker

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// loopbackClientThrough reaches the host's loopback-TCP HTTP-CONNECT front the
// way nono's upstream_proxy does: dial the TCP port, send the CONNECT preamble,
// then TLS with the request-derived SNI (trusting the host CA).
func loopbackClientThrough(t *testing.T, h *Host) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(h.CACertPEM()) {
		t.Fatal("append CA")
	}
	addr := fmt.Sprintf("127.0.0.1:%d", h.LoopbackPort())
	return &http.Client{
		Transport: &http.Transport{
			ForceAttemptHTTP2: false,
			TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
			DialTLSContext: func(ctx context.Context, network, dialAddr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(dialAddr)
				if err != nil {
					host = dialAddr
				}
				raw, err := net.Dial("tcp", addr)
				if err != nil {
					return nil, err
				}
				fmt.Fprintf(raw, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", host, host)
				br := bufio.NewReader(raw)
				line, err := br.ReadString('\n')
				if err != nil || !strings.Contains(line, "200") {
					raw.Close()
					return nil, fmt.Errorf("CONNECT failed: %q %v", line, err)
				}
				for {
					l, err := br.ReadString('\n')
					if err != nil || l == "\r\n" || l == "\n" {
						break
					}
				}
				tc := tls.Client(raw, &tls.Config{ServerName: host, RootCAs: pool, NextProtos: []string{"http/1.1"}})
				if err := tc.Handshake(); err != nil {
					raw.Close()
					return nil, err
				}
				return tc, nil
			},
		},
	}
}

// TestLoopbackFrontDisabledByDefault: without LoopbackFront, no loopback port is
// bound (LoopbackPort() == 0) and only the srt unix front serves.
func TestLoopbackFrontDisabledByDefault(t *testing.T) {
	h, _ := startHost(t, Config{SessionID: "s", EmptyPathScope: "allow"})
	if h.LoopbackPort() != 0 {
		t.Errorf("LoopbackPort() = %d, want 0 when LoopbackFront is off", h.LoopbackPort())
	}
}

// TestLoopbackFrontServesAndInjects: with LoopbackFront enabled, Start binds a
// nonzero loopback port and the front injects end-to-end.
func TestLoopbackFrontServesAndInjects(t *testing.T) {
	h, up := startHost(t, Config{SessionID: "s", EmptyPathScope: "allow", LoopbackFront: true})
	if h.LoopbackPort() == 0 {
		t.Fatal("LoopbackPort() = 0 with LoopbackFront enabled")
	}
	c := loopbackClientThrough(t, h)

	resp, err := c.Get("https://api.github.com/repos/o/r")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if up.lastAuth() != "Bearer READ-TOK" {
		t.Errorf("upstream auth = %q, want Bearer READ-TOK", up.lastAuth())
	}
	if strings.Contains(string(body), "READ-TOK") {
		t.Errorf("response leaked token")
	}
}

// TestBothFrontsServeSameProxy: srt unix front AND loopback TCP front both inject
// on one Host (dual-front during the transition).
func TestBothFrontsServeSameProxy(t *testing.T) {
	h, up := startHost(t, Config{SessionID: "s", EmptyPathScope: "allow", LoopbackFront: true})

	// srt unix front.
	unix := clientThrough(t, h)
	resp, err := unix.Get("https://api.github.com/repos/o/r")
	if err != nil {
		t.Fatalf("unix GET: %v", err)
	}
	resp.Body.Close()
	if up.lastAuth() != "Bearer READ-TOK" {
		t.Errorf("unix front auth = %q", up.lastAuth())
	}

	// loopback TCP front.
	lb := loopbackClientThrough(t, h)
	resp, err = lb.Get("https://api.github.com/repos/o/r")
	if err != nil {
		t.Fatalf("loopback GET: %v", err)
	}
	resp.Body.Close()
	if up.lastAuth() != "Bearer READ-TOK" {
		t.Errorf("loopback front auth = %q", up.lastAuth())
	}
}

// TestLoopbackFrontClosesClean: Close tears down BOTH fronts without hanging and
// the loopback port stops accepting.
func TestLoopbackFrontClosesClean(t *testing.T) {
	h, _ := startHost(t, Config{SessionID: "s", EmptyPathScope: "allow", LoopbackFront: true})
	port := h.LoopbackPort()

	done := make(chan struct{})
	go func() { h.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Host.Close hung with the loopback front enabled")
	}

	// The loopback port must no longer accept a connection.
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err == nil {
		c.Close()
		t.Errorf("loopback port %d still accepting after Close", port)
	}
}
