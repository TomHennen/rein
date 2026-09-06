package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testSecret = "s3cr3t-per-run"

// tcpHarness is a harness serving the #185 TCP listener with a policy whose
// resolver maps names onto the harness's own fake GitHub server (loopback,
// test-permitted) so raw tunnels have somewhere to go.
func tcpHarness(t *testing.T, mode EgressMode, domains []string) *harness {
	t.Helper()
	pol, err := NewEgressPolicy(mode, domains, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pol.permitLoopback = true
	h := newHarness(t, harnessOpts{egress: pol, proxySecret: testSecret, probeNonce: "nonce-123"})
	// Point every tunnel target at the fake GitHub httptest server, and make
	// its port the policy's default so "host:<that port>" plays the 443 role.
	ghHost, ghPort, _ := net.SplitHostPort(h.ghAddr())
	pol.defaultPort, _ = strconv.Atoi(ghPort)
	pol.Resolve = fakeResolver(map[string][]string{
		"allowed.example": {ghHost}, "other.example": {ghHost}, "example.com": {ghHost},
	})
	h.pol = pol
	return h
}

// egressResolve replaces the harness policy's resolver map.
func (h *harness) egressResolve(m map[string][]string) {
	h.pol.Resolve = fakeResolver(m)
}

// tgt renders host:<the test default port>.
func (h *harness) tgt(host string) string {
	_, port, _ := net.SplitHostPort(h.ghAddr())
	return host + ":" + port
}

// connect opens a TCP connection to the proxy and sends a CONNECT with the
// given auth header value ("" = none). Returns the raw conn, its reader, and
// the CONNECT response.
func connect(t *testing.T, h *harness, target, auth string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(h.tcpPort), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	hdr := ""
	if auth != "" {
		hdr = "Proxy-Authorization: " + auth + "\r\n"
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, target, hdr)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("CONNECT %s: %v", target, err)
	}
	return c, br, resp
}

func body(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b)
}

func TestTCP_RequiresProxySecret(t *testing.T) {
	h := tcpHarness(t, EgressOpen, nil)
	_, _, resp := connect(t, h, h.tgt("example.com"), "")
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("no auth: status = %d, want 407", resp.StatusCode)
	}
	_, _, resp = connect(t, h, h.tgt("example.com"), proxyAuthValue("wrong"))
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("wrong secret: status = %d, want 407", resp.StatusCode)
	}
}

func TestTCP_SOCKSAndPlaintextRefused(t *testing.T) {
	h := tcpHarness(t, EgressOpen, nil)
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(h.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte{0x05, 0x01, 0x00}) // SOCKS5 greeting
	reply := make([]byte, 2)
	if _, err := io.ReadFull(c, reply); err != nil || reply[0] != 0x05 || reply[1] != 0xff {
		t.Errorf("SOCKS greeting reply = %v err=%v, want 05 FF", reply, err)
	}

	c2, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(h.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_ = c2.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c2, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\nProxy-Authorization: %s\r\n\r\n", proxyAuthValue(testSecret))
	resp, err := http.ReadResponse(bufio.NewReader(c2), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body(resp), "plain http") {
		t.Errorf("plaintext GET: status = %d", resp.StatusCode)
	}
}

// TestTCP_RawTunnelOpenAndRestricted: an allowed CONNECT yields a raw tunnel
// through which the client does its OWN TLS to the upstream (rein never
// terminates or injects); a refused one gets the 403 naming the remedy.
func TestTCP_RawTunnelOpenAndRestricted(t *testing.T) {
	h := tcpHarness(t, EgressRestricted, []string{"allowed.example"})
	c, br, resp := connect(t, h, h.tgt("allowed.example"), proxyAuthValue(testSecret))
	defer c.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowed CONNECT: status = %d body=%s", resp.StatusCode, body(resp))
	}
	tc := tls.Client(&prefixConn{r: br, Conn: c}, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("client TLS through the tunnel: %v", err)
	}
	fmt.Fprintf(tc, "GET /repos/o/r/pulls HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	up, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if up.StatusCode != 200 {
		t.Errorf("upstream through tunnel: %d", up.StatusCode)
	}
	if got := h.gh.last().Auth; got != "" {
		t.Errorf("raw tunnel carried an Authorization header %q; rein must never inject on this path", got)
	}

	_, _, resp = connect(t, h, h.tgt("other.example"), proxyAuthValue(testSecret))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unlisted host: status = %d", resp.StatusCode)
	}
	if b := body(resp); !strings.Contains(b, "rein session allow-domain other.example") {
		t.Errorf("refusal must name the remedy: %s", b)
	}

	o := tcpHarness(t, EgressOpen, nil)
	c3, _, resp := connect(t, o, o.tgt("other.example"), proxyAuthValue(testSecret))
	c3.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("open mode: status = %d", resp.StatusCode)
	}
	_, _, resp = connect(t, o, "other.example:8443", proxyAuthValue(testSecret))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("open mode non-443 port: status = %d", resp.StatusCode)
	}
	// (Loopback refusal in open mode is pinned by TestEgressDecide_NeverRouteIsUnconditional;
	// this harness permits loopback so it has an upstream to tunnel to.)
}

// TestTCP_InjectHostsTerminateAndPinSNI: a CONNECT to an inject host is
// terminated and injected exactly as on the unix socket; a non-443 port is
// refused; a CONNECT host whose SNI differs is refused (two-key rule).
func TestTCP_InjectHostsTerminateAndPinSNI(t *testing.T) {
	h := tcpHarness(t, EgressRestricted, nil)
	c, br, resp := connect(t, h, "api.github.com:443", proxyAuthValue(testSecret))
	defer c.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT api.github.com: %d", resp.StatusCode)
	}
	tc := tls.Client(&prefixConn{r: br, Conn: c}, &tls.Config{ServerName: "api.github.com", RootCAs: h.caPool, NextProtos: []string{"http/1.1"}})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake with rein's leaf: %v", err)
	}
	fmt.Fprintf(tc, "GET /repos/o/r/pulls HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	up, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if up.StatusCode != 200 || !strings.Contains(h.gh.last().Auth, "Bearer") {
		t.Errorf("inject via TCP: status=%d auth=%q", up.StatusCode, h.gh.last().Auth)
	}

	_, _, resp = connect(t, h, "api.github.com:8443", proxyAuthValue(testSecret))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("inject host on 8443: status = %d", resp.StatusCode)
	}

	c2, br2, resp := connect(t, h, "github.com:443", proxyAuthValue(testSecret))
	defer c2.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	tc2 := tls.Client(&prefixConn{r: br2, Conn: c2}, &tls.Config{ServerName: "api.github.com", RootCAs: h.caPool, NextProtos: []string{"http/1.1"}})
	if err := tc2.Handshake(); err == nil {
		fmt.Fprintf(tc2, "GET /repos/o/r/pulls HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
		if _, err := http.ReadResponse(bufio.NewReader(tc2), nil); err == nil {
			t.Error("CONNECT github.com with SNI api.github.com was served; the two-key rule must refuse it")
		}
	}
}

// TestTCP_CDNRawTunnelInRestrictedMode: a CDN host is a raw tunnel (no rein
// TLS, no token) even when the operator listed nothing.
func TestTCP_CDNRawTunnelInRestrictedMode(t *testing.T) {
	h := tcpHarness(t, EgressRestricted, nil)
	ghHost, _, _ := net.SplitHostPort(h.ghAddr())
	h.egressResolve(map[string][]string{"codeload.github.com": {ghHost}})
	c, br, resp := connect(t, h, h.tgt("codeload.github.com"), proxyAuthValue(testSecret))
	defer c.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CDN CONNECT: status = %d body=%s", resp.StatusCode, body(resp))
	}
	tc := tls.Client(&prefixConn{r: br, Conn: c}, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("client TLS through the CDN tunnel: %v", err)
	}
	fmt.Fprintf(tc, "GET /o/r/tarball HTTP/1.1\r\nHost: codeload.github.com\r\n\r\n")
	if _, err := http.ReadResponse(bufio.NewReader(tc), nil); err != nil {
		t.Fatal(err)
	}
	if got := h.gh.last().Auth; got != "" {
		t.Errorf("CDN tunnel carried Authorization %q", got)
	}
}

// TestTCP_UnauthenticatedGetsOnly407: before the secret, every request shape
// (plaintext GET included) gets the bare 407, not a rein-identifying 403.
func TestTCP_UnauthenticatedGetsOnly407(t *testing.T) {
	h := tcpHarness(t, EgressOpen, nil)
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(h.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("unauthenticated plaintext GET: status = %d, want 407", resp.StatusCode)
	}
}

// TestTCP_TunnelHalfClose: a client that shuts its write side after sending
// its request must still receive the response (the half-close reaches the
// TCP upstream instead of tearing the tunnel down).
func TestTCP_TunnelHalfClose(t *testing.T) {
	// A plain-TCP echo upstream: reads until EOF, then writes what it got.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				c.Write(append([]byte("echo:"), b...))
			}()
		}
	}()
	upHost, upPort, _ := net.SplitHostPort(ln.Addr().String())
	pol, err := NewEgressPolicy(EgressOpen, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pol.permitLoopback = true
	pol.defaultPort, _ = strconv.Atoi(upPort)
	pol.Resolve = fakeResolver(map[string][]string{"echo.example": {upHost}})
	h := newHarness(t, harnessOpts{egress: pol, proxySecret: testSecret, probeNonce: "n"})

	c, br, resp := connect(t, h, "echo.example:"+upPort, proxyAuthValue(testSecret))
	defer c.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %d", resp.StatusCode)
	}
	c.Write([]byte("half"))
	c.(*net.TCPConn).CloseWrite() // client half-closes after its request
	got, err := io.ReadAll(br)
	if err != nil || string(got) != "echo:half" {
		t.Errorf("after half-close got %q err=%v, want echo:half", got, err)
	}
}

// TestTCP_TunnelBound: the 257th concurrent raw tunnel is refused with 429.
func TestTCP_TunnelBound(t *testing.T) {
	h := tcpHarness(t, EgressOpen, nil)
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < maxConcurrentTunnels; i++ {
		c, _, resp := connect(t, h, h.tgt("other.example"), proxyAuthValue(testSecret))
		held = append(held, c)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("tunnel %d: status = %d", i, resp.StatusCode)
		}
	}
	_, _, resp := connect(t, h, h.tgt("other.example"), proxyAuthValue(testSecret))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("tunnel %d: status = %d, want 429", maxConcurrentTunnels+1, resp.StatusCode)
	}
}

// TestTCP_SNIMismatchIsAudited: CONNECT github.com but SNI api.github.com is
// refused AND recorded.
func TestTCP_SNIMismatchIsAudited(t *testing.T) {
	pol, err := NewEgressPolicy(EgressRestricted, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	audit := &syncBuffer{}
	h := newHarnessWithAudit(t, harnessOpts{egress: pol, proxySecret: testSecret, probeNonce: "n"}, audit)
	c, br, resp := connect(t, h, "github.com:443", proxyAuthValue(testSecret))
	defer c.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	tc := tls.Client(&prefixConn{r: br, Conn: c}, &tls.Config{ServerName: "api.github.com", RootCAs: h.caPool, NextProtos: []string{"http/1.1"}})
	_ = tc.Handshake()
	fmt.Fprintf(tc, "GET /repos/o/r/pulls HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	if _, err := http.ReadResponse(bufio.NewReader(tc), nil); err == nil {
		t.Error("mismatched SNI was served")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(audit.String(), "refused-connect-sni-mismatch") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(audit.String(), "refused-connect-sni-mismatch") {
		t.Errorf("audit lacks the mismatch refusal:\n%s", audit.String())
	}
}

func TestTCP_ProbeHostAnswersWithNonce(t *testing.T) {
	h := tcpHarness(t, EgressRestricted, nil)
	_, _, resp := connect(t, h, ProbeHost+":443", proxyAuthValue(testSecret))
	if resp.StatusCode != http.StatusOK || resp.Header.Get(ProbeHeader) != "nonce-123" {
		t.Errorf("probe: status=%d header=%q", resp.StatusCode, resp.Header.Get(ProbeHeader))
	}
	_, _, resp = connect(t, h, ProbeHost+":8443", proxyAuthValue(testSecret))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("probe on 8443: %d", resp.StatusCode)
	}
}

// TestTCP_UnconfiguredRefusesEverything: a proxy without an egress policy or
// secret serves nothing on TCP (fail closed).
func TestTCP_UnconfiguredRefusesEverything(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	tln, port, err := ListenTCP()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &Proxy{logger: testLogger(t), audit: NewAuditLog(io.Discard), handshakeTimeout: time.Second}
	go p.ServeTCP(ctx, tln)
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	if n, _ := c.Read(make([]byte, 1)); n != 0 {
		t.Error("unconfigured TCP listener answered")
	}
	_ = h
}
