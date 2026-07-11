package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"testing"
)

// TestALPNPinnedToHTTP11 pins the NextProtos guard in handleConn
// (conformance audit #44 §2): the relay loop is http.ReadRequest-based and
// cannot parse h2 frames, so the TLS server must negotiate http/1.1 even
// when the client PREFERS h2. Every other test in this suite offers only
// http/1.1 itself, so deleting the pin would leave the suite green while a
// real client (git/curl/gh all offer h2 first) negotiated h2 and got a
// garbage relay.
func TestALPNPinnedToHTTP11(t *testing.T) {
	h := newHarness(t, harnessOpts{})

	raw, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatalf("dial proxy socket: %v", err)
	}
	defer raw.Close()

	tc := tls.Client(raw, &tls.Config{
		ServerName: "api.github.com",
		RootCAs:    h.caPool,
		// h2 first: a client that prefers HTTP/2, like real git/curl/gh.
		NextProtos: []string{"h2", "http/1.1"},
	})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	if got := tc.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		t.Fatalf("NegotiatedProtocol = %q, want %q (the proxy must refuse h2)", got, "http/1.1")
	}

	// And the negotiated connection actually serves an HTTP/1.1 request —
	// the pin is load-bearing for the relay, not just a handshake detail.
	fmt.Fprintf(tc, "GET /repos/o/r HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatalf("read response over the negotiated conn: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 from the fake upstream", resp.StatusCode)
	}
}
