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
	"sync"
	"sync/atomic"
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

	// HandshakeTimeout bounds the inbound pre-request window (CONNECT preamble
	// + TLS handshake). Zero uses defaultHandshakeTimeout. (C1)
	HandshakeTimeout time.Duration
	// IdleTimeout bounds how long we wait for the NEXT request's line+headers
	// on a kept-alive connection. It does NOT bound the body upload or a human
	// approval pause (those come after the request is read). Zero uses
	// defaultIdleTimeout. (C1)
	IdleTimeout time.Duration

	// OnActivity, if set, is called once per handled request (before the
	// decision) — the per-request signal the run-level idle/hard-TTL expiry
	// monitor (runbroker) uses to know the session is still active. Must be
	// cheap and non-blocking (it runs inline on the request path). Nil = no
	// activity signal (tests / expiry disabled).
	OnActivity func()
}

// Proxy is one session's TLS-terminating injecting relay. Serve it on a
// listener (from Listen) per accepted connection.
type Proxy struct {
	sessionID        string
	core             *brokercore.Core
	ca               *CA
	audit            *AuditLog
	logger           *log.Logger
	client           *http.Client
	handshakeTimeout time.Duration
	idleTimeout      time.Duration
	onActivity       func()
}

// Inbound deadline defaults (C1). Handshake ~10s matches the upstream
// TLSHandshakeTimeout. Idle 5min bounds the gap BETWEEN requests on a
// kept-alive connection while staying comfortably above git's own pauses; it
// never bounds the body upload or a human approval pause (both happen after the
// request line+headers are read, when we clear the read deadline).
const (
	defaultHandshakeTimeout = 10 * time.Second
	defaultIdleTimeout      = 5 * time.Minute
)

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
	handshakeTimeout := cfg.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = defaultHandshakeTimeout
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultIdleTimeout
	}
	return &Proxy{
		sessionID:        cfg.SessionID,
		core:             cfg.Core,
		ca:               cfg.CA,
		audit:            cfg.Audit,
		logger:           cfg.Logger,
		handshakeTimeout: handshakeTimeout,
		idleTimeout:      idleTimeout,
		onActivity:       cfg.OnActivity,
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

// Serve accepts connections on ln until ctx is cancelled or ln is closed, then
// returns nil ONLY after every in-flight connection has been closed and its
// handler has returned. Cancelling the context is a hard stop: the capability
// (design §5.3) must not outlive the run, so accepted connections — which hold
// an already-granted write approval and can keep minting — are force-closed,
// not merely drained. The caller's Close relies on this to know the socket and
// all token traffic are truly gone on return.
//
// Contract: the caller MUST cancel ctx to fully shut down. The ctx-watcher
// goroutine below blocks on ctx.Done; if a caller closes ln WITHOUT cancelling
// ctx, Serve returns but that goroutine lingers until ctx is eventually
// cancelled. runbroker.Host.Close always does both (cancel then close), so this
// is a documented contract, not a leak in practice.
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	var (
		mu      sync.Mutex
		conns   = map[net.Conn]struct{}{}
		closing bool
		wg      sync.WaitGroup
	)
	go func() {
		<-ctx.Done()
		mu.Lock()
		closing = true
		ln.Close()
		for c := range conns {
			c.Close() // kills the TLS read loop → handleConn returns
		}
		mu.Unlock()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				wg.Wait() // block until every in-flight handler has finished
				return nil
			}
			// Transient accept error — back off so a persistent one doesn't
			// busy-loop the CPU.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		mu.Lock()
		if closing {
			mu.Unlock()
			conn.Close()
			continue
		}
		conns[conn] = struct{}{}
		wg.Add(1)
		mu.Unlock()
		go func() {
			defer wg.Done()
			defer func() {
				mu.Lock()
				delete(conns, conn)
				mu.Unlock()
			}()
			p.handleConn(conn)
		}()
	}
}

// handleConn consumes any srt CONNECT preamble, terminates TLS (leaf keyed on
// SNI), then serves HTTP/1.1 requests. SNI is the single identity source
// (design §4.1): the upstream connection AND the injection host class both
// derive from it, and each request's Host header is validated against it.
func (p *Proxy) handleConn(conn net.Conn) {
	defer conn.Close()

	// Deadline for the whole pre-request window (CONNECT preamble + TLS
	// handshake). A hostile in-sandbox agent could open the socket, send the
	// CONNECT line, then never send a ClientHello — without a deadline that
	// pins a goroutine + fd in the TRUSTED broker process forever; repeated a
	// few thousand times it exhausts them. (C1)
	_ = conn.SetDeadline(time.Now().Add(p.handshakeTimeout))

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
	// Handshake done — clear the pre-request deadline; serveRequests manages
	// per-request idle deadlines from here.
	_ = conn.SetDeadline(time.Time{})

	// Normalize the SNI ONCE (lowercase + trim the trailing FQDN dot) and use
	// that single value everywhere downstream — upstream URL, host class,
	// tier classification, repo extraction, graphql gate. This removes a whole
	// class of case/dot mismatch (e.g. the old `sni == "api.github.com"`
	// graphql gate missed `API.github.com.`). (L1)
	sni := strings.ToLower(strings.TrimSuffix(tlsConn.ConnectionState().ServerName, "."))
	if sni == "" {
		// No SNI ⇒ no identity ⇒ we can't safely inject or route. Fail closed.
		p.logger.Printf("connection with no SNI; refusing")
		return
	}
	p.serveRequests(tlsConn, sni)
}

// serveRequests runs the keep-alive request loop against a TLS-terminated
// connection whose identity host is the already-normalized sni.
func (p *Proxy) serveRequests(conn net.Conn, sni string) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		// Idle deadline: bound how long we wait for the NEXT request line +
		// headers, so a stalled/slowloris client can't hold the connection
		// (and its goroutine/fd) open indefinitely. (C1) We CLEAR it once the
		// request is read: the body upload and any human write-approval pause
		// happen after ReadRequest returns and are legitimately unbounded — a
		// large git push and a person deciding are both slow, and the approval
		// pause never overlaps ReadRequest (it runs later, inside serveOne).
		_ = conn.SetReadDeadline(time.Now().Add(p.idleTimeout))
		req, err := http.ReadRequest(r)
		if err != nil {
			if err != io.EOF && !errors.Is(err, net.ErrClosed) {
				p.logger.Printf("read request (sni=%q): %v", sni, err)
			}
			return
		}
		_ = conn.SetReadDeadline(time.Time{}) // unbounded past headers (body + approval)
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
	// Run-level activity signal (idle-expiry monitor). Fired for EVERY handled
	// request — including refused ones — so a misbehaving-but-active agent still
	// counts as activity (idle means "no proxy traffic at all", not "no allowed
	// traffic"). Cheap + non-blocking by contract.
	if p.onActivity != nil {
		p.onActivity()
	}
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
		p.logger.Printf("host %q not in the allowed GitHub set; refusing", sni)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Decision: "refused-host"})
		p.writeLocalError(conn, http.StatusForbidden, "rein: host not allowed for this session")
		return false
	}

	switch class {
	case classPassthrough:
		// CDN / asset hosts: relay verbatim, NEVER inject. The client's own
		// Authorization (if any) passes through untouched — we neither add ours
		// nor strip theirs (design §4.3). No scope/approval decision, so it is
		// safe to invite the body now.
		if !p.handleExpectContinue(conn, req) {
			return false
		}
		return p.relay(conn, req, sni, "", "passthrough", "")

	case classInjectBearer, classInjectBasic:
		return p.serveInject(conn, req, sni, class)
	}
	return false
}

// serveInject classifies the tier, gets a credential decision, and relays with
// the host-appropriate injected auth — or answers locally on refusal.
//
// Ordering matters for Expect: 100-continue (C2): for path/method-classified
// requests (git smart-HTTP, REST) we make the FULL scope/approval/mint decision
// with NO body, and only send "100 Continue" once we've decided to relay — so a
// refused or unapproved push receives the 403/502 and is NEVER invited to
// upload its (possibly huge) pack, and on the CP4 approval path git doesn't
// stream the pack while the human is still deciding. GraphQL is the one case
// whose tier needs the body, so there we must send 100 first and buffer — but
// that body is capped at 1 MiB, so the pre-decision upload is bounded.
func (p *Proxy) serveInject(conn net.Conn, req *http.Request, sni string, class hostClass) bool {
	isGraphQL := sni == "api.github.com" && isGraphQLPath(req.URL.Path)

	var body []byte
	if isGraphQL {
		// Need the body to classify query-vs-mutation → invite it first, then
		// buffer (capped). Read one past the cap to DETECT oversize and reject
		// (413) rather than truncate-and-desync.
		if !p.handleExpectContinue(conn, req) {
			return false
		}
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
		// forward upstream (fail closed), and never with a token. For a
		// non-graphql request we have NOT sent 100 Continue, so a client using
		// Expect will get this 403 instead of being invited to upload (C2).
		p.logger.Printf("refused: sni=%q repo=%q tier=%s reason=%q", sni, repo, tier, reason)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier.String(), Decision: "refused-scope"})
		p.writeLocalError(conn, http.StatusForbidden,
			"rein: this repository is out of the session's scope, or a write was not approved. Run `rein doctor`.")
		return false
	case brokercore.PlaceholderMintFailed:
		p.logger.Printf("mint failed: sni=%q repo=%q tier=%s", sni, repo, tier)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier.String(), Decision: "mint-failed"})
		p.writeLocalError(conn, http.StatusBadGateway,
			"rein: could not mint a GitHub token for this request. Run `rein doctor`.")
		return false
	}

	// Decision is "relay". For a non-graphql request the body has NOT been
	// invited yet — send 100 Continue now, AFTER the scope/approval/mint
	// decision, so only an authorized request uploads its pack (C2). (GraphQL
	// already answered Expect above.)
	if !isGraphQL {
		if !p.handleExpectContinue(conn, req) {
			return false
		}
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
	// Expect: 100-continue was already answered by the caller (the passthrough
	// arm, or serveInject after its scope/approval/mint decision), BEFORE any
	// body read. Upstream still gets Expect stripped via the hop-by-hop set.
	//
	// Wrap the inbound body to track whether it reached EOF. If upstream
	// responds WITHOUT reading the whole request body — GitHub 401/403'ing an
	// unauthorized push is exactly this — the leftover bytes are still sitting
	// in the shared bufio.Reader that the NEXT http.ReadRequest would read, and
	// net/http may drain them from a background goroutine: a data race plus a
	// desynced keep-alive stream. So if the body wasn't fully consumed by the
	// time client.Do returns, we close the connection instead of reusing it.
	var body *eofBody
	if req.Body != nil {
		body = &eofBody{rc: req.Body}
	}
	var reqBody io.Reader
	if body != nil {
		reqBody = body
	}
	out, err := http.NewRequest(req.Method, "https://"+sni+req.URL.RequestURI(), reqBody)
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
		p.logger.Printf("%s %q %q -> upstream error: %v", req.Method, sni, req.URL.Path, err)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier, Decision: decision, Status: http.StatusBadGateway})
		p.writeLocalError(conn, http.StatusBadGateway, "rein: upstream request failed")
		return false
	}
	p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier, Decision: decision, Status: resp.StatusCode})

	stripHopByHopHeaders(resp.Header) // symmetric with the request direction (F4)
	werr := resp.Write(conn)
	resp.Body.Close()
	if werr != nil {
		return false // client-side write failed; conn may be desynced — drop it (CP1 recipe point 6)
	}
	if req.Close || resp.Close {
		return false
	}
	// If upstream didn't consume the whole request body, leftover bytes remain
	// in the shared reader (and net/http may still be draining them) — reusing
	// the connection would race the next ReadRequest and desync the stream.
	// Fail safe: close. (F1)
	if body != nil && !body.reachedEOF() {
		p.logger.Printf("request body not fully consumed by upstream (status %d); closing connection to avoid keep-alive desync", resp.StatusCode)
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

// hopByHopSet returns the RFC 7230 §6.1 hop-by-hop header set, plus any token
// named in the given Connection header value (which is itself hop-by-hop).
func hopByHopSet(connectionHeader string) map[string]bool {
	hop := map[string]bool{
		"connection": true, "proxy-connection": true, "expect": true,
		"keep-alive": true, "te": true, "trailer": true, "upgrade": true,
		// RFC 7230 §6.1 also lists the proxy-auth pair as hop-by-hop.
		"proxy-authorization": true, "proxy-authenticate": true,
	}
	for _, tok := range strings.Split(connectionHeader, ",") {
		if t := strings.ToLower(strings.TrimSpace(tok)); t != "" {
			hop[t] = true
		}
	}
	return hop
}

// copyHeadersStripHopByHop copies src into dst minus the hop-by-hop headers
// (CP1 recipe point 5), used on the REQUEST direction (client → upstream).
func copyHeadersStripHopByHop(src, dst http.Header) {
	hop := hopByHopSet(src.Get("Connection"))
	for k, vs := range src {
		if hop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// stripHopByHopHeaders deletes hop-by-hop headers from h in place, used on the
// RESPONSE direction (upstream → client) so upstream's Connection/Keep-Alive/
// TE/etc. aren't forwarded verbatim to the sandbox client (F4).
func stripHopByHopHeaders(h http.Header) {
	for k := range hopByHopSet(h.Get("Connection")) {
		// Delete uses canonical form; range keys are lower-cased set entries.
		h.Del(k)
	}
}

// eofBody wraps the inbound request body to record whether it was read to EOF
// (i.e. fully consumed by the upstream client). Access to the flag is atomic
// because net/http may read the body from a background goroutine (F1).
type eofBody struct {
	rc  io.ReadCloser
	eof atomic.Bool
}

func (b *eofBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if err == io.EOF {
		b.eof.Store(true)
	}
	return n, err
}

func (b *eofBody) Close() error     { return b.rc.Close() }
func (b *eofBody) reachedEOF() bool { return b.eof.Load() }

// prefixConn lets tls.Server read bytes the CONNECT-preamble sniff already
// buffered into br, then continue reading from the underlying conn.
type prefixConn struct {
	r io.Reader
	net.Conn
}

func (c *prefixConn) Read(b []byte) (int, error) { return c.r.Read(b) }
