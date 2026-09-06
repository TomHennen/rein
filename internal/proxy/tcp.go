// The TCP listener: srt's external-proxy shape (#185). srt bridges the
// sandbox's fixed proxy port to this loopback port and enforces nothing
// itself, so everything on this path is rein's: peer-uid check, the per-run
// proxy secret, SOCKS refusal, CONNECT-only, then the two-key class rule —
// the CONNECT target picks the class and the policy; for terminated classes
// the SNI must equal the CONNECT host.
package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProxyUser is the userinfo name in the in-sandbox proxy URL (srt's own
// convention, kept so tools that special-case it keep working).
const ProxyUser = "srt"

// ListenTCP binds the external-proxy listener on the literal loopback address
// (srt's bridge dials TCP:localhost:<port>; the peer check needs the exact
// tuple) and returns it with its port.
func ListenTCP() (net.Listener, int, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, 0, fmt.Errorf("proxy: listen tcp: %w", err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

// ServeTCP runs the external-proxy accept loop until ctx is done.
func (p *Proxy) ServeTCP(ctx context.Context, ln net.Listener) error {
	return p.serveWith(ctx, ln, p.handleTCPConn)
}

// proxyAuthValue is the exact Proxy-Authorization value the secret yields.
func proxyAuthValue(secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(ProxyUser+":"+secret))
}

// ProxyURL renders the in-sandbox proxy URL carrying the secret (what
// sandbox-exec writes into HTTPS_PROXY).
func ProxyURL(secret string, port int) string {
	return fmt.Sprintf("http://%s:%s@localhost:%d", ProxyUser, secret, port)
}

func (p *Proxy) handleTCPConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(p.handshakeTimeout))
	audit := func(decision, host string) {
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: host, Method: http.MethodConnect, Decision: decision})
	}
	if p.egress == nil || p.proxySecret == "" {
		audit("refused-egress-unconfigured", "")
		return
	}
	// Peer-uid check BEFORE any byte is read (defense in depth behind the
	// secret: the bridge socket's mode follows srt's umask).
	if err := peerUIDMatches(conn, os.Getuid()); err != nil {
		p.logger.Printf("tcp: refusing peer %s: %v", conn.RemoteAddr(), err)
		audit("refused-peer", conn.RemoteAddr().String())
		return
	}
	br := bufio.NewReader(conn)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	if first[0] == 0x04 || first[0] == 0x05 {
		// A SOCKS greeting: single-protocol port. 05 FF = no acceptable method.
		_, _ = conn.Write([]byte{0x05, 0xff})
		audit("refused-egress-socks", "")
		return
	}
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		// No plaintext relay: it could only ever be a route toward the inject
		// path with a token on the wire.
		audit("refused-egress-plaintext", req.Host)
		p.writeLocalJSON(conn, http.StatusForbidden, "rein: only CONNECT (https) is proxied; plain http:// is refused")
		return
	}
	if req.Header.Get("Proxy-Authorization") != proxyAuthValue(p.proxySecret) {
		audit("refused-proxy-auth", req.Host)
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"rein\"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	target := strings.ToLower(strings.TrimSpace(req.Host))
	if target == "" {
		target = strings.ToLower(req.RequestURI)
	}
	host, port, err := splitHostPortStrict(target)
	if err != nil {
		audit("refused-egress-syntax", target)
		p.writeLocalJSON(conn, http.StatusBadRequest, "rein: CONNECT target must be host:port")
		return
	}
	host = strings.TrimSuffix(host, ".")
	hp := net.JoinHostPort(host, strconv.Itoa(port))

	// Class by CONNECT target. rein's own hosts are 443-only, always.
	if reservedHost(host) {
		if port != egressDefaultPort {
			audit("refused-egress-port", hp)
			p.writeLocalJSON(conn, http.StatusForbidden, fmt.Sprintf("rein: %s is reachable on port 443 only", host))
			return
		}
		switch {
		case host == ProbeHost:
			// Launch self-test: prove THIS rein answers, not a leftover proxy.
			audit("probe", hp)
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			fmt.Fprintf(conn, "HTTP/1.1 200 Connection Established\r\n%s: %s\r\n\r\n", ProbeHeader, p.probeNonce)
			return
		case classifyHost(host) == classPassthrough:
			// CDN: an always-allowed raw tunnel (the agent sees GitHub's real
			// certificate), still never-route-checked.
			dec := p.egress.Decide(context.Background(), host, port)
			if !dec.Allowed && !strings.HasSuffix(dec.Reason, "-host") {
				audit(dec.Reason, hp)
				p.writeLocalJSON(conn, http.StatusForbidden, dec.Message)
				return
			}
			dec.Allowed, dec.Reason = true, "allowed-egress-cdn"
			p.tunnel(conn, br, dec, hp, port)
			return
		default:
			// Inject + virtual hosts: terminate; the SNI must equal this host.
			if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
				return
			}
			p.terminateTLS(conn, br, host)
			return
		}
	}

	dec := p.egress.Decide(context.Background(), host, port)
	if !dec.Allowed {
		audit(dec.Reason, hp)
		p.writeLocalJSON(conn, http.StatusForbidden, dec.Message)
		return
	}
	p.tunnel(conn, br, dec, hp, port)
}

// tunnel dials the checked addresses and splices raw bytes, under the run's
// concurrency bound and an idle timeout.
func (p *Proxy) tunnel(conn net.Conn, br *bufio.Reader, dec EgressDecision, hp string, port int) {
	if !p.tunnels.acquire() {
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: hp, Method: http.MethodConnect, Decision: "refused-egress-tunnel-limit"})
		p.writeLocalJSON(conn, http.StatusTooManyRequests, fmt.Sprintf("rein: more than %d concurrent tunnels", maxConcurrentTunnels))
		return
	}
	defer p.tunnels.release()
	ctx, cancel := context.WithTimeout(context.Background(), egressDialTimeout)
	up, err := p.egress.dialChecked(ctx, dec, port)
	cancel()
	if err != nil {
		p.logger.Printf("tunnel %s: dial: %v", hp, err)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: hp, Method: http.MethodConnect, Decision: "refused-egress-dial"})
		p.writeLocalJSON(conn, http.StatusBadGateway, "rein: could not connect to "+hp)
		return
	}
	defer up.Close()
	p.audit.Record(AuditEntry{Session: p.sessionID, Host: hp, Method: http.MethodConnect, Decision: dec.Reason})
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	splice(&idleConn{Conn: &prefixConn{r: br, Conn: conn}, idle: egressIdleTimeout}, &idleConn{Conn: up, idle: egressIdleTimeout})
}

// idleConn re-arms a read deadline on every read so a tunnel with no traffic
// in either direction for `idle` closes (each side's reader is the other's
// writer, so activity in either direction resets one of them).
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(b)
}

// tunnelGate bounds concurrent raw tunnels per run.
type tunnelGate struct {
	mu sync.Mutex
	n  int
}

func (g *tunnelGate) acquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n >= maxConcurrentTunnels {
		return false
	}
	g.n++
	return true
}

func (g *tunnelGate) release() {
	g.mu.Lock()
	g.n--
	g.mu.Unlock()
}
