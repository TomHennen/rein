package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/brokercore"
	"github.com/TomHennen/rein/internal/classify"
)

// Config builds a Proxy. Core, CA, and Logger are required; Audit and Upstream
// default safely.
type Config struct {
	// SessionID tags audit lines.
	SessionID string
	// Core is this session's decision core (build via NewSessionCore so it has
	// the per-session read/write caches and approval memo).
	Core *brokercore.Core
	// CA terminates TLS with a per-SNI leaf.
	CA *CA
	// Audit records token-redacted decisions. Nil = no audit.
	Audit *AuditLog
	// Logger receives forensic lines (never a token). Required.
	Logger *log.Logger
	// Upstream is the transport used to reach the real GitHub hosts. Nil uses
	// the CP1-recipe transport (HTTP/1.1, no compression, system roots). Tests
	// inject a transport that redirects github.com:443 to an httptest server.
	Upstream http.RoundTripper
}

// Proxy is one session's TLS-terminating injecting relay. Serve it on a
// listener (from Listen) per accepted connection.
type Proxy struct {
	sessionID string
	core      *brokercore.Core
	ca        *CA
	audit     *AuditLog
	logger    *log.Logger
	client    *http.Client
}

// New constructs a Proxy from cfg.
func New(cfg Config) (*Proxy, error) {
	if cfg.Core == nil {
		return nil, errors.New("proxy: Core is required")
	}
	if cfg.CA == nil {
		return nil, errors.New("proxy: CA is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("proxy: Logger is required")
	}
	transport := cfg.Upstream
	if transport == nil {
		transport = defaultTransport()
	}
	return &Proxy{
		sessionID: cfg.SessionID,
		core:      cfg.Core,
		ca:        cfg.CA,
		audit:     cfg.Audit,
		logger:    cfg.Logger,
		client: &http.Client{
			// No global timeout: a large git push can legitimately run long,
			// and the CP1 recipe requires a transparent relay. Failures surface
			// as upstream errors that drop the connection, never a client hang.
			Transport: transport,
			// Relay 3xx verbatim instead of following it (CP1 recipe point 2):
			// following redirects swallows ones git expects, 502s a redirected
			// POST (GetBody nil), and drops injected auth cross-host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// defaultTransport is the CP1-recipe upstream transport: HTTP/1.1 only, no
// transparent compression (so Go doesn't gunzip and break Content-Length),
// system-root TLS.
func defaultTransport() *http.Transport {
	return &http.Transport{
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// Serve accepts connections on ln until ctx is cancelled or ln is closed.
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			// Transient accept error — back off so a persistent one doesn't
			// busy-loop the CPU.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		go p.handleConn(conn)
	}
}

// handleConn consumes any srt CONNECT preamble, terminates TLS (leaf keyed on
// SNI), then serves HTTP/1.1 requests. SNI is the single identity source
// (design §4.1): the upstream connection AND the injection host class both
// derive from it, and each request's Host header is validated against it.
func (p *Proxy) handleConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)

	// srt forwards the matched-domain CONNECT as an opaque tunnel: it writes
	// `CONNECT host:443` + headers, expects a 200, then pipes the raw client
	// TLS. Consume the preamble if present; a direct TLS client (no CONNECT) is
	// also supported. The CONNECT host is NOT trusted for identity — SNI is.
	if peek, _ := br.Peek(8); string(peek) == "CONNECT " {
		if _, err := br.ReadString('\n'); err != nil {
			return
		}
		for { // drain remaining preamble headers to the blank line
			line, err := br.ReadString('\n')
			if err != nil || line == "\r\n" || line == "\n" {
				break
			}
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
	}

	tlsConn := tls.Server(&prefixConn{r: br, Conn: conn}, &tls.Config{
		GetCertificate: p.ca.getLeaf,
		NextProtos:     []string{"http/1.1"}, // pin: http.ReadRequest can't parse h2 (design §4.1)
	})
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	sni := tlsConn.ConnectionState().ServerName
	if sni == "" {
		// No SNI ⇒ no identity ⇒ we can't safely inject or route. Fail closed.
		p.logger.Printf("connection with no SNI; refusing")
		return
	}
	p.serveRequests(tlsConn, sni)
}

// serveRequests runs the keep-alive request loop against a TLS-terminated
// connection whose identity host is sni.
func (p *Proxy) serveRequests(conn net.Conn, sni string) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		req, err := http.ReadRequest(r)
		if err != nil {
			if err != io.EOF && !errors.Is(err, net.ErrClosed) {
				p.logger.Printf("read request (sni=%s): %v", sni, err)
			}
			return
		}
		keepAlive := p.serveOne(conn, req, sni)
		if !keepAlive {
			return
		}
	}
}

// serveOne handles a single request and reports whether the connection may be
// reused for a further request. A false return closes the connection (used for
// refusals and any relay error, to avoid a desynced keep-alive stream).
func (p *Proxy) serveOne(conn net.Conn, req *http.Request, sni string) (keepAlive bool) {
	// Identity: the plaintext Host header MUST match the TLS SNI (design §4.1).
	// Otherwise the agent could open a connection "to github.com" and steer an
	// injected token at an attacker-chosen Host.
	if !hostMatchesSNI(req.Host, sni) {
		p.logger.Printf("Host %q != SNI %q; refusing", req.Host, sni)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Decision: "refused-host-mismatch"})
		p.writeLocalError(conn, http.StatusBadRequest, "rein: Host header does not match TLS SNI; refusing to relay")
		return false
	}

	class := classifyHost(sni)
	if class == classRefuse {
		p.logger.Printf("host %q not in the allowed GitHub set; refusing (sni=%s)", sni, sni)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Decision: "refused-host"})
		p.writeLocalError(conn, http.StatusForbidden, "rein: host not allowed for this session")
		return false
	}

	// From here the request WILL have its body read/relayed. Honor
	// Expect: 100-continue BEFORE any body read (CP1 recipe point 4) — this
	// must precede serveInject's GraphQL buffering, or a graphql POST that
	// waits for 100 before sending its body deadlocks. We only send 100 for a
	// host we're going to serve (never for the refuse/mismatch cases above,
	// where the client should see the error and abort, not upload a body).
	if !p.handleExpectContinue(conn, req) {
		return false
	}

	switch class {
	case classPassthrough:
		// CDN / asset hosts: relay verbatim, NEVER inject. The client's own
		// Authorization (if any) passes through untouched — we neither add ours
		// nor strip theirs (design §4.3).
		return p.relay(conn, req, sni, "", "passthrough", "")

	case classInjectBearer, classInjectBasic:
		return p.serveInject(conn, req, sni, class)
	}
	return false
}

// serveInject classifies the tier, gets a credential decision, and relays with
// the host-appropriate injected auth — or answers locally on refusal.
func (p *Proxy) serveInject(conn net.Conn, req *http.Request, sni string, class hostClass) bool {
	// GraphQL is the only case that needs the body to classify (query vs
	// mutation). Buffer it (queries are small) and re-attach so the relay still
	// forwards it. Everything else is path/method-classified with a nil body —
	// never buffer a git pack just to classify.
	var body []byte
	if sni == "api.github.com" && isGraphQLPath(req.URL.Path) {
		// Read one byte past the cap to DETECT an oversized body and reject it
		// (413), rather than silently truncating and relaying a corrupted
		// request. GraphQL documents are small in practice. Expect:100-continue
		// was already answered in serveOne, so this read can't deadlock.
		const maxGraphQL = 1 << 20
		var err error
		body, err = io.ReadAll(io.LimitReader(req.Body, maxGraphQL+1))
		req.Body.Close()
		if err != nil {
			p.writeLocalError(conn, http.StatusBadGateway, "rein: could not read request body")
			return false
		}
		if len(body) > maxGraphQL {
			p.writeLocalError(conn, http.StatusRequestEntityTooLarge, "rein: graphql request body too large to classify")
			return false
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}

	tier, reason := classify.Classify(sni, req.Method, req.URL.Path, req.URL.RawQuery, body)
	repo := requestRepo(sni, req.URL.Path)

	cred := p.core.Serve(context.Background(), brokercore.Request{
		Repo:        repo,
		WriteIntent: tier == classify.Write,
	})

	switch cred.Password {
	case brokercore.PlaceholderRefused:
		// Out of scope, or write approval denied. Answer locally — do NOT
		// forward upstream (fail closed), and never with a token.
		p.logger.Printf("refused: sni=%s repo=%q tier=%s reason=%q", sni, repo, tier, reason)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier.String(), Decision: "refused-scope"})
		p.writeLocalError(conn, http.StatusForbidden,
			"rein: this repository is out of the session's scope, or a write was not approved. Run `rein doctor`.")
		return false
	case brokercore.PlaceholderMintFailed:
		p.logger.Printf("mint failed: sni=%s repo=%q tier=%s", sni, repo, tier)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier.String(), Decision: "mint-failed"})
		p.writeLocalError(conn, http.StatusBadGateway,
			"rein: could not mint a GitHub token for this request. Run `rein doctor`.")
		return false
	}

	// Real token in hand: inject the host-appropriate scheme and relay
	// (spike-verified: Bearer for api/uploads, Basic x-access-token for the
	// github.com git transport — Bearer 401s there).
	authValue := "Bearer " + cred.Password
	if class == classInjectBasic {
		authValue = "Basic " + base64.StdEncoding.EncodeToString([]byte(brokercore.CredentialUsername+":"+cred.Password))
	}
	return p.relay(conn, req, sni, authValue, "inject", tier.String())
}

// relay forwards req to the real upstream host (sni) and streams the response
// back. authValue, when non-empty, OVERWRITES the Authorization header
// (host-aware injection). decision/tier feed the audit line. Returns whether
// the connection may be reused.
func (p *Proxy) relay(conn net.Conn, req *http.Request, sni, authValue, decision, tier string) (keepAlive bool) {
	// Expect: 100-continue was already answered in serveOne, BEFORE any body
	// read (including serveInject's GraphQL buffering). Upstream still gets
	// Expect stripped via the hop-by-hop set below.
	out, err := http.NewRequest(req.Method, "https://"+sni+req.URL.RequestURI(), req.Body)
	if err != nil {
		p.writeLocalError(conn, http.StatusBadGateway, "rein: could not build upstream request")
		return false
	}
	// CP1 recipe point 1 (load-bearing): http.NewRequest with an opaque body
	// leaves ContentLength=0 / no TransferEncoding, truncating the receive-pack
	// POST. Carry the inbound framing explicitly.
	out.ContentLength = req.ContentLength
	out.TransferEncoding = req.TransferEncoding
	out.Host = sni

	copyHeadersStripHopByHop(req.Header, out.Header)
	if authValue != "" {
		out.Header.Set("Authorization", authValue) // overwrite (covers a dummy GH_TOKEN)
	}

	resp, err := p.client.Do(out)
	if err != nil {
		p.logger.Printf("%s %s%s -> upstream error: %v", req.Method, sni, req.URL.Path, err)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier, Decision: decision, Status: http.StatusBadGateway})
		p.writeLocalError(conn, http.StatusBadGateway, "rein: upstream request failed")
		return false
	}
	p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier, Decision: decision, Status: resp.StatusCode})

	werr := resp.Write(conn)
	resp.Body.Close()
	if werr != nil {
		return false // client-side write failed; conn may be desynced — drop it (CP1 recipe point 6)
	}
	if req.Close || resp.Close {
		return false
	}
	return true
}

// handleExpectContinue answers an Expect: 100-continue request by writing
// "100 Continue" and dropping the header, so the client sends its body. It MUST
// be called before any body read on the connection (CP1 recipe point 4).
// Returns false only on a socket write error (the caller drops the connection).
func (p *Proxy) handleExpectContinue(conn net.Conn, req *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(req.Header.Get("Expect")), "100-continue") {
		return true
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
		return false
	}
	req.Header.Del("Expect")
	return true
}

// writeLocalError writes a short plaintext HTTP error toward the client and
// closes the connection (Connection: close). The body is a fixed "rein: …"
// string — it NEVER contains a token (design §4.1 response-path hygiene).
func (p *Proxy) writeLocalError(conn net.Conn, status int, msg string) {
	body := msg + "\n"
	fmt.Fprintf(conn,
		"HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

// hostMatchesSNI reports whether the request Host header names the same host as
// the TLS SNI, allowing an explicit :443 (the default HTTPS port). A missing
// Host does not match — fail closed.
func hostMatchesSNI(host, sni string) bool {
	if host == "" {
		return false
	}
	if h, port, err := net.SplitHostPort(host); err == nil {
		if port != "443" {
			return false
		}
		host = h
	}
	return strings.EqualFold(host, sni)
}

// copyHeadersStripHopByHop copies src into dst minus the RFC 7230 §6.1
// hop-by-hop headers plus any token named in the inbound Connection header
// (CP1 recipe point 5).
func copyHeadersStripHopByHop(src, dst http.Header) {
	hop := map[string]bool{
		"connection": true, "proxy-connection": true, "expect": true,
		"keep-alive": true, "te": true, "trailer": true, "upgrade": true,
		// RFC 7230 §6.1 also lists the proxy-auth pair as hop-by-hop; a client
		// Proxy-Authorization must not be relayed to GitHub.
		"proxy-authorization": true, "proxy-authenticate": true,
	}
	for _, tok := range strings.Split(src.Get("Connection"), ",") {
		if t := strings.ToLower(strings.TrimSpace(tok)); t != "" {
			hop[t] = true
		}
	}
	for k, vs := range src {
		if hop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// prefixConn lets tls.Server read bytes the CONNECT-preamble sniff already
// buffered into br, then continue reading from the underlying conn.
type prefixConn struct {
	r io.Reader
	net.Conn
}

func (c *prefixConn) Read(b []byte) (int, error) { return c.r.Read(b) }
