package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// dialLoopbackTLS connects to the loopback-TCP front the way nono's
// upstream_proxy does: open the TCP port, send the `CONNECT host:443` preamble,
// read the 200, then start TLS with the given SNI (trusting the rein CA).
func (h *harness) dialLoopbackTLS(sni string) (*tls.Conn, error) {
	raw, err := net.Dial("tcp", h.tcpAddr)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(raw, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", sni, sni)
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
	tc := tls.Client(&prefixConn{r: br, Conn: raw}, &tls.Config{
		ServerName: sni,
		RootCAs:    h.caPool,
		NextProtos: []string{"http/1.1"},
	})
	if err := tc.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	return tc, nil
}

// loopbackClient reaches the loopback-TCP front, deriving SNI from the request
// URL host. Redirects are surfaced, not followed.
func (h *harness) loopbackClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			ForceAttemptHTTP2:     false,
			TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
			ExpectContinueTimeout: 10 * time.Second,
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					host = addr
				}
				return h.dialLoopbackTLS(host)
			},
		},
	}
}

// TestListenLoopbackBindsLoopback is the (a) coverage: ListenLoopback binds an
// IPv4 loopback address (never 0.0.0.0) and hands back a usable OS-chosen port.
func TestListenLoopbackBindsLoopback(t *testing.T) {
	ln, port, err := ListenLoopback(0)
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	defer ln.Close()
	if port == 0 {
		t.Fatal("port 0 returned; OS-assigned port not surfaced")
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("addr type = %T, want *net.TCPAddr", ln.Addr())
	}
	if !tcp.IP.IsLoopback() {
		t.Errorf("bound IP %v is not loopback", tcp.IP)
	}
	if tcp.IP.Equal(net.IPv4zero) || tcp.IP.Equal(net.IPv6zero) {
		t.Errorf("bound to wildcard address %v", tcp.IP)
	}
	if tcp.Port != port {
		t.Errorf("returned port %d != listener port %d", port, tcp.Port)
	}
	// The reported port must actually be dialable on loopback.
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatalf("dial returned port: %v", err)
	}
	c.Close()
}

// TestLoopbackFrontInjectsBearer is the (b) coverage: a request through the
// loopback-TCP CONNECT front reaches the fake upstream with the injected token
// on an inject host, and the token never appears on the response.
func TestLoopbackFrontInjectsBearer(t *testing.T) {
	h := newHarness(t, harnessOpts{loopback: true})
	c := h.loopbackClient()

	resp, body := doGET(t, c, "https://api.github.com/repos/o/r/pulls")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := h.gh.last().Auth; got != "Bearer "+h.readTok {
		t.Errorf("api auth = %q, want Bearer read token", got)
	}
	if strings.Contains(body, h.readTok) {
		t.Errorf("response body leaked token")
	}
}

// TestLoopbackFrontCDNNoToken: a CDN host through the loopback front is relayed
// verbatim and NEVER injected (no token leaks to a pre-signed asset URL).
func TestLoopbackFrontCDNNoToken(t *testing.T) {
	h := newHarness(t, harnessOpts{loopback: true})
	c := h.loopbackClient()

	doGET(t, c, "https://codeload.github.com/o/r/tar.gz/main")
	if got := h.gh.last().Auth; got != "" {
		t.Errorf("CDN got injected auth %q through loopback front, want none", got)
	}
}

// TestLoopbackFrontChunkedPushStreams: a large chunked receive-pack body streams
// through the loopback front intact (the exact case nono's own inject path
// hangs/413s on), and the declare/receive-pack write tap mints a write token.
func TestLoopbackFrontChunkedPushStreams(t *testing.T) {
	h := newHarness(t, harnessOpts{loopback: true})
	c := h.loopbackClient()

	const size = 1500 * 1024 // > 1 MiB, forces the chunked relay path
	payload := bytes.Repeat([]byte("g"), size)
	req, _ := http.NewRequest("POST", "https://github.com/o/r.git/git-receive-pack", bytes.NewReader(payload))
	req.TransferEncoding = []string{"chunked"}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("push POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("push status = %d", resp.StatusCode)
	}
	last := h.gh.last()
	if last.BodyN != size {
		t.Errorf("upstream received %d body bytes, want %d", last.BodyN, size)
	}
	if !strings.HasPrefix(last.Auth, "Basic ") {
		t.Errorf("push auth = %q, want Basic", last.Auth)
	}
}

// TestLoopbackFrontDeclareTap: the receive-pack declare gate fires through the
// loopback front — an undeclared push is refused locally, nothing reaches
// upstream, nothing is minted.
func TestLoopbackFrontDeclareTap(t *testing.T) {
	confirmed := false
	h := newHarness(t, harnessOpts{
		loopback: true,
		decl: &DeclarationHooks{
			WriteApproved:  func(string) bool { return confirmed },
			IssueConfirmed: func(string, int) bool { return confirmed },
		},
	})
	c := h.loopbackClient()

	req, _ := http.NewRequest("POST", "https://github.com/o/r.git/git-receive-pack", strings.NewReader("packdata"))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("push POST: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if h.gh.count() != 0 {
		t.Errorf("undeclared push reached upstream through loopback front")
	}
	if got := h.gh.last().Auth; got != "" {
		t.Errorf("undeclared push minted/forwarded a token: %q", got)
	}
}

// TestLoopbackFrontSNIHostMismatch: the SNI==Host invariant holds on the
// loopback front too — a Host header disagreeing with the SNI is refused before
// any upstream contact.
func TestLoopbackFrontSNIHostMismatch(t *testing.T) {
	h := newHarness(t, harnessOpts{loopback: true})
	tc, err := h.dialLoopbackTLS("github.com")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer tc.Close()
	br := bufio.NewReader(tc)
	// SNI is github.com but the Host header says api.github.com.
	fmt.Fprintf(tc, "GET /o/r.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	tc.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on Host!=SNI", resp.StatusCode)
	}
	if h.gh.count() != 0 {
		t.Errorf("Host-mismatch request reached upstream")
	}
}

// TestSrtFrontUnbrokenAlongsideLoopback: with BOTH fronts live on one proxy, the
// srt unix-socket front still injects correctly (dual-front, no regression).
func TestSrtFrontUnbrokenAlongsideLoopback(t *testing.T) {
	h := newHarness(t, harnessOpts{loopback: true})
	unix := h.httpClient(true) // srt CONNECT preamble over the unix socket

	resp, _ := doGET(t, unix, "https://api.github.com/repos/o/r")
	if resp.StatusCode != 200 {
		t.Fatalf("srt-front status = %d", resp.StatusCode)
	}
	if got := h.gh.last().Auth; got != "Bearer "+h.readTok {
		t.Errorf("srt front auth = %q, want Bearer read token", got)
	}
}
