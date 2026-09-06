package srt

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// The #185 half of the in-sandbox probe: prove the sandbox's proxy port leads
// to THIS rein (nonce), that the network namespace is still unshared (a raw
// public dial fails), and that the host's loopback is unreachable (a raw dial
// to rein's own port fails). All three are answered in milliseconds inside an
// unshared namespace; none needs the internet.

// Probe exit codes for the network checks (continue the Probe* numbering).
const (
	// ProbeEgressNotRein: the CONNECT to probe.rein.internal did not come back
	// with this run's nonce — srt's own proxy (or a stale rein) answered, so
	// the external-proxy wiring did not take and srt would be enforcing (or
	// not enforcing) egress instead of rein.
	ProbeEgressNotRein = 14
	// ProbeNetNotUnshared: a raw TCP connect to a public address succeeded
	// from inside the sandbox — the network namespace is shared with the host.
	ProbeNetNotUnshared = 15
	// ProbeLoopbackReachable: a raw connect to 127.0.0.1:<rein port> succeeded
	// from inside — the agent can reach host loopback services directly.
	ProbeLoopbackReachable = 16
)

// probeNetwork runs the three checks. proxyURL is the in-sandbox HTTPS_PROXY
// (srt sets http://localhost:3128 with no userinfo in the external shape);
// secret is REIN_PROXY_AUTH; nonce is the expected X-Rein-Probe value;
// reinPort is rein's host loopback port.
func probeNetwork(proxyURL, secret, nonce string, reinPort int) int {
	if err := probeRein(proxyURL, secret, nonce); err != nil {
		fmt.Fprintf(os.Stderr, "probe: egress is not rein: %v\n", err)
		return ProbeEgressNotRein
	}
	if c, err := net.DialTimeout("tcp", "1.1.1.1:443", 2*time.Second); err == nil {
		c.Close()
		return ProbeNetNotUnshared
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(reinPort)), 2*time.Second); err == nil {
		c.Close()
		return ProbeLoopbackReachable
	}
	return ProbeOK
}

// probeRein sends one authenticated CONNECT for the probe host through the
// proxy at proxyURL and requires the nonce header on the 200.
func probeRein(proxyURL, secret, nonce string) error {
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("bad proxy url %q", proxyURL)
	}
	c, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("srt:"+secret))
	fmt.Fprintf(c, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\nProxy-Authorization: %s\r\n\r\n", probeHost, probeHost, auth)
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CONNECT %s: %s (%s)", probeHost, resp.Status, resp.Header.Get("X-Proxy-Error"))
	}
	if got := resp.Header.Get(probeHeader); got != nonce {
		return errors.New("nonce mismatch: another proxy answered")
	}
	return nil
}

// Mirrors proxy.ProbeHost / proxy.ProbeHeader; kept as literals so the srt
// package does not import the proxy package's egress machinery into the probe.
const (
	probeHost   = "probe.rein.internal"
	probeHeader = "X-Rein-Probe"
)
