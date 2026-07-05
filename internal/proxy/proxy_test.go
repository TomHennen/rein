package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/keystore"
)

// capturedReq is what the fake upstream GitHub recorded for one request.
type capturedReq struct {
	Host   string
	Method string
	Path   string
	Auth   string
	BodyN  int
}

// fakeGitHub stands in for the real GitHub hosts. All GitHub SNI hosts are
// redirected here by the test upstream transport; r.Host tells them apart.
type fakeGitHub struct {
	mu   sync.Mutex
	reqs []capturedReq
	// respond, if set, customizes the response per request (e.g. a 302 or a
	// body). Default: 200 "upstream-ok".
	respond func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n, _ := io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, capturedReq{
		Host:   r.Host,
		Method: r.Method,
		Path:   r.URL.Path,
		Auth:   r.Header.Get("Authorization"),
		BodyN:  int(n),
	})
	f.mu.Unlock()
	if f.respond != nil {
		f.respond(w, r)
		return
	}
	io.WriteString(w, "upstream-ok")
}

func (f *fakeGitHub) last() capturedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return capturedReq{}
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeGitHub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// harness wires a proxy in front of a fake GitHub over a unix socket.
type harness struct {
	t          *testing.T
	ca         *CA
	socket     string
	caPool     *x509.CertPool
	gh         *fakeGitHub
	readCalls  int32
	writeCalls int32
	approvals  int32
	readTok    string
	writeTok   string
}

type harnessOpts struct {
	repos            []string          // session scope ceiling; nil ⇒ InScope allows all
	approve          func(string) bool // write approval hook; nil ⇒ auto-approve
	respond          func(http.ResponseWriter, *http.Request)
	mintWErr         error         // if set, MintWrite returns this error
	auditW           io.Writer     // if set, the proxy writes its audit log here
	handshakeTimeout time.Duration // if set, overrides the inbound handshake deadline
	idleTimeout      time.Duration // if set, overrides the inbound idle deadline
}

// syncBuffer is a goroutine-safe bytes.Buffer for capturing the audit log,
// which the proxy writes from connection-handler goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newHarnessWithAudit(t *testing.T, opts harnessOpts, w io.Writer) *harness {
	opts.auditW = w
	return newHarness(t, opts)
}

func newHarness(t *testing.T, opts harnessOpts) *harness {
	t.Helper()
	h := &harness{t: t, readTok: "READ-TOKEN-secret", writeTok: "WRITE-TOKEN-secret"}

	gh := &fakeGitHub{respond: opts.respond}
	srv := httptest.NewUnstartedServer(gh)
	srv.EnableHTTP2 = false
	srv.StartTLS()
	t.Cleanup(srv.Close)
	h.gh = gh

	ghURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Upstream transport: redirect every GitHub host to the test server; the
	// leg to httptest won't validate ServerName github.com, so skip verify —
	// injection/relay is what's under test, not this hop.
	upstream := &http.Transport{
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
		TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", ghURL.Host)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	// Keystore-backed CA in a temp dir.
	ks := keystore.NewFileKeystore(t.TempDir())
	ca, err := LoadOrCreateCA(ks)
	if err != nil {
		t.Fatal(err)
	}
	h.ca = ca
	h.caPool = x509.NewCertPool()
	if !h.caPool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("append CA cert to pool failed")
	}

	var inScope func(string) bool
	if opts.repos != nil {
		set := map[string]bool{}
		for _, r := range opts.repos {
			set[strings.ToLower(r)] = true
		}
		inScope = func(repo string) bool {
			// mimic session.Contains normalization enough for tests
			r := strings.ToLower(strings.TrimSuffix(repo, ".git"))
			return set[r]
		}
	}

	mintRead := func(ctx context.Context) (string, time.Time, error) {
		atomic.AddInt32(&h.readCalls, 1)
		return h.readTok, time.Now().Add(time.Hour), nil
	}
	mintWrite := func(ctx context.Context) (string, time.Time, error) {
		atomic.AddInt32(&h.writeCalls, 1)
		if opts.mintWErr != nil {
			return "", time.Time{}, opts.mintWErr
		}
		return h.writeTok, time.Now().Add(time.Hour), nil
	}
	approve := opts.approve
	wrappedApprove := func(repo string) bool {
		atomic.AddInt32(&h.approvals, 1)
		if approve != nil {
			return approve(repo)
		}
		return true
	}

	core := NewSessionCore(SessionConfig{
		SessionID:      "sess_test",
		MintRead:       mintRead,
		MintWrite:      mintWrite,
		InScope:        inScope,
		Approve:        wrappedApprove,
		ReadCache:      NewMemCache(),
		EmptyPathScope: "allow",
		Logger:         testLogger(t),
	})

	var audit *AuditLog
	if opts.auditW != nil {
		audit = NewAuditLog(opts.auditW)
	}
	p, err := New(Config{
		SessionID:        "sess_test",
		Core:             core,
		CA:               ca,
		Audit:            audit,
		Logger:           testLogger(t),
		Upstream:         upstream,
		HandshakeTimeout: opts.handshakeTimeout,
		IdleTimeout:      opts.idleTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}

	h.socket = filepath.Join(t.TempDir(), "proxy.sock")
	ln, err := Listen(h.socket, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Serve(ctx, ln)
	return h
}

func testLogger(t *testing.T) *log.Logger {
	return log.New(&testWriter{t}, "", 0)
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(b []byte) (int, error) {
	w.t.Logf("proxy: %s", strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// dialProxyTLS opens a TLS connection to the proxy over its unix socket,
// impersonating an in-sandbox client with the given SNI. When useConnect is
// set it first sends the srt CONNECT preamble.
func (h *harness) dialProxyTLS(sni string, useConnect bool) (*tls.Conn, error) {
	raw, err := net.Dial("unix", h.socket)
	if err != nil {
		return nil, err
	}
	if useConnect {
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
	}
	tc := tls.Client(raw, &tls.Config{
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

// httpClient builds an http.Client that reaches the proxy over the unix socket,
// deriving SNI from the request URL host. Redirects are surfaced (not followed)
// so tests can assert 3xx passes through.
func (h *harness) httpClient(useConnect bool) *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			ForceAttemptHTTP2: false,
			TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
			// Make Expect:100-continue load-bearing: with a non-zero timeout the
			// client WAITS for the proxy's 100 before sending the body (Go's
			// zero-value sends immediately, which would mask a missing 100).
			ExpectContinueTimeout: 10 * time.Second,
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					host = addr
				}
				return h.dialProxyTLS(host, useConnect)
			},
		},
	}
}

func doGET(t *testing.T, c *http.Client, rawurl string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(rawurl)
	if err != nil {
		t.Fatalf("GET %s: %v", rawurl, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// --- tests ---

func TestInjectBearerVsBasic(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.httpClient(false)

	// api.github.com REST GET → Bearer, read tier.
	resp, body := doGET(t, c, "https://api.github.com/repos/o/r/pulls")
	if resp.StatusCode != 200 {
		t.Fatalf("api status = %d", resp.StatusCode)
	}
	if got := h.gh.last().Auth; got != "Bearer "+h.readTok {
		t.Errorf("api auth = %q, want Bearer read token", got)
	}
	if strings.Contains(body, h.readTok) {
		t.Errorf("response body leaked token")
	}

	// github.com git fetch advertisement → Basic x-access-token, read tier.
	doGET(t, c, "https://github.com/o/r.git/info/refs?service=git-upload-pack")
	last := h.gh.last()
	if !strings.HasPrefix(last.Auth, "Basic ") {
		t.Errorf("github auth = %q, want Basic", last.Auth)
	}
	if last.Host != "github.com" {
		t.Errorf("github upstream host = %q, want github.com", last.Host)
	}
	if atomic.LoadInt32(&h.readCalls) == 0 {
		t.Errorf("read mint never called")
	}
	if atomic.LoadInt32(&h.writeCalls) != 0 {
		t.Errorf("write mint called on read path")
	}
}

func TestSNIHostMismatchRejected(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.httpClient(false)
	req, _ := http.NewRequest("GET", "https://github.com/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Host = "api.github.com" // Host header disagrees with SNI (github.com)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if h.gh.count() != 0 {
		t.Fatalf("mismatched request reached upstream (%d reqs)", h.gh.count())
	}
}

func TestNeverInjectCDNPassthrough(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.httpClient(false)

	// No Authorization from client → none should appear upstream.
	doGET(t, c, "https://codeload.github.com/o/r/tar.gz/main")
	if got := h.gh.last().Auth; got != "" {
		t.Errorf("CDN got injected auth %q, want none", got)
	}
	if atomic.LoadInt32(&h.readCalls)+atomic.LoadInt32(&h.writeCalls) != 0 {
		t.Errorf("CDN passthrough minted a token")
	}

	// Client's own Authorization passes through UNTOUCHED (design §4.3).
	req, _ := http.NewRequest("GET", "https://objects.githubusercontent.com/x", nil)
	req.Header.Set("Authorization", "Bearer client-supplied")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := h.gh.last().Auth; got != "Bearer client-supplied" {
		t.Errorf("CDN auth = %q, want the client's own untouched", got)
	}
}

func TestHopByHopHeadersStripped(t *testing.T) {
	// Capture the full upstream header set for this case.
	var gotHeaders http.Header
	h := newHarness(t, harnessOpts{
		respond: func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			io.WriteString(w, "ok")
		},
	})
	c := h.httpClient(false)
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/o/r", nil)
	// A connection-token'd custom header plus a proxy-auth header must not reach
	// upstream (RFC 7230 §6.1 hop-by-hop, CP1 recipe point 5).
	req.Header.Set("Connection", "X-Custom-Hop")
	req.Header.Set("X-Custom-Hop", "secret")
	req.Header.Set("Proxy-Authorization", "Basic sniff")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotHeaders.Get("X-Custom-Hop") != "" {
		t.Errorf("Connection-listed header X-Custom-Hop reached upstream")
	}
	if gotHeaders.Get("Proxy-Authorization") != "" {
		t.Errorf("Proxy-Authorization reached upstream")
	}
	if gotHeaders.Get("Connection") != "" {
		t.Errorf("Connection header reached upstream")
	}
}

func TestRefuseUnknownHost(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.httpClient(false)
	resp, body := doGET(t, c, "https://evil.example.com/steal")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if h.gh.count() != 0 {
		t.Fatalf("refused host reached upstream")
	}
	if !strings.HasPrefix(body, "rein:") {
		t.Errorf("body = %q, want rein: prefix", body)
	}
}

func TestChunkedPostBodyIntact(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.httpClient(false)

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
	if atomic.LoadInt32(&h.writeCalls) == 0 {
		t.Errorf("receive-pack did not mint a write token")
	}
}

func TestExpect100Continue(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.httpClient(false)
	body := bytes.Repeat([]byte("x"), 4096)
	req, _ := http.NewRequest("POST", "https://github.com/o/r.git/git-receive-pack", bytes.NewReader(body))
	req.Header.Set("Expect", "100-continue")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("expect-continue POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if h.gh.last().BodyN != len(body) {
		t.Errorf("body bytes = %d, want %d", h.gh.last().BodyN, len(body))
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	h := newHarness(t, harnessOpts{
		respond: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://codeload.github.com/o/r/pack")
			w.WriteHeader(http.StatusFound) // 302
		},
	})
	c := h.httpClient(false)
	resp, _ := doGET(t, c, "https://github.com/o/r.git/info/refs?service=git-upload-pack")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 relayed verbatim", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://codeload.github.com/o/r/pack" {
		t.Errorf("Location = %q, not relayed", loc)
	}
	if h.gh.count() != 1 {
		t.Errorf("upstream saw %d requests; the relay must not follow the redirect", h.gh.count())
	}
}

func TestRefusedScopeLocal403(t *testing.T) {
	h := newHarness(t, harnessOpts{repos: []string{"allowed/repo"}})
	c := h.httpClient(false)
	resp, body := doGET(t, c, "https://github.com/other/repo.git/info/refs?service=git-upload-pack")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if h.gh.count() != 0 {
		t.Fatalf("out-of-scope request reached upstream")
	}
	if strings.Contains(body, h.readTok) || strings.Contains(body, h.writeTok) {
		t.Errorf("refusal body leaked a token")
	}
	if !strings.HasPrefix(body, "rein:") {
		t.Errorf("body = %q, want rein: explanation", body)
	}
}

func TestMintFailedLocal502(t *testing.T) {
	h := newHarness(t, harnessOpts{mintWErr: fmt.Errorf("boom")})
	c := h.httpClient(false)
	req, _ := http.NewRequest("POST", "https://github.com/o/r.git/git-receive-pack", strings.NewReader("data"))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if h.gh.count() != 0 {
		t.Fatalf("mint-failed request reached upstream")
	}
	if !strings.Contains(string(body), "rein doctor") {
		t.Errorf("502 body = %q, want doctor hint", body)
	}
}

func TestOneApprovalCoversInfoRefsAndReceivePack(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.httpClient(false)

	// info/refs?service=git-receive-pack (write advertisement) then the
	// receive-pack POST — a single push. One approval, one mint must cover both.
	doGET(t, c, "https://github.com/o/r.git/info/refs?service=git-receive-pack")
	req, _ := http.NewRequest("POST", "https://github.com/o/r.git/git-receive-pack", strings.NewReader("packdata"))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := atomic.LoadInt32(&h.approvals); got != 1 {
		t.Errorf("approvals = %d, want 1 (run-scoped, per repo)", got)
	}
	if got := atomic.LoadInt32(&h.writeCalls); got != 1 {
		t.Errorf("write mints = %d, want 1 (cached across the push)", got)
	}
}

func TestApprovalDeniedRefuses(t *testing.T) {
	h := newHarness(t, harnessOpts{approve: func(string) bool { return false }})
	c := h.httpClient(false)
	req, _ := http.NewRequest("POST", "https://github.com/o/r.git/git-receive-pack", strings.NewReader("x"))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 on denied approval", resp.StatusCode)
	}
	if h.gh.count() != 0 {
		t.Fatalf("denied write reached upstream")
	}
	if atomic.LoadInt32(&h.writeCalls) != 0 {
		t.Fatalf("denied write still minted")
	}
}

func TestLeafCachedAcrossConnections(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.httpClient(false)
	doGET(t, c, "https://github.com/o/r.git/info/refs?service=git-upload-pack")
	if !h.ca.leafCached("github.com") {
		t.Fatalf("leaf for github.com not cached after first request")
	}
	// A second, fresh connection must reuse the cached leaf (same pointer).
	first, _ := h.ca.getLeaf(&tls.ClientHelloInfo{ServerName: "github.com"})
	second, _ := h.ca.getLeaf(&tls.ClientHelloInfo{ServerName: "github.com"})
	if first != second {
		t.Errorf("leaf re-minted; cache miss on second connection")
	}
}

func TestConnectPreambleSupported(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.httpClient(true) // send the srt CONNECT preamble
	resp, _ := doGET(t, c, "https://api.github.com/repos/o/r")
	if resp.StatusCode != 200 {
		t.Fatalf("status via CONNECT = %d", resp.StatusCode)
	}
	if h.gh.last().Auth != "Bearer "+h.readTok {
		t.Errorf("CONNECT path did not inject Bearer")
	}
}

func TestGraphQLMutationIsWrite(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.httpClient(false)
	q := `{"query":"mutation { addStar(input:{starrableId:\"x\"}) { clientMutationId } }"}`
	req, _ := http.NewRequest("POST", "https://api.github.com/graphql", strings.NewReader(q))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&h.writeCalls) == 0 {
		t.Errorf("graphql mutation did not mint a write token")
	}
	// The buffered body must still reach upstream intact.
	if h.gh.last().BodyN != len(q) {
		t.Errorf("graphql body bytes = %d, want %d", h.gh.last().BodyN, len(q))
	}
}

// TestHandshakeTimeoutReclaimsConn is the C1 guard: a client that sends the
// CONNECT preamble then NEVER sends a ClientHello must be timed out and its
// connection reclaimed, not pinned forever.
func TestHandshakeTimeoutReclaimsConn(t *testing.T) {
	h := newHarness(t, harnessOpts{handshakeTimeout: 250 * time.Millisecond})
	raw, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Send the CONNECT preamble, read the 200, then stall (no ClientHello).
	fmt.Fprintf(raw, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
	br := bufio.NewReader(raw)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "200") {
		t.Fatalf("CONNECT reply = %q err=%v", line, err)
	}
	for { // drain to blank line
		l, err := br.ReadString('\n')
		if err != nil || l == "\r\n" || l == "\n" {
			break
		}
	}
	// The proxy must close the connection after the handshake deadline; a read
	// returns EOF/err well before a generous ceiling.
	raw.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("connection still open after handshake deadline; goroutine/fd would leak")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("connection reclaimed after %s, want ~handshake timeout", elapsed)
	}
}

// TestExpectRefusedBeforeUpload is the C2 guard: an out-of-scope push carrying
// Expect: 100-continue must receive the 403 WITHOUT being invited to upload its
// body (no "100 Continue" first, and upstream sees nothing).
func TestExpectRefusedBeforeUpload(t *testing.T) {
	h := newHarness(t, harnessOpts{repos: []string{"allowed/repo"}})
	tc := h.rawTLS(t, "github.com")
	defer tc.Close()
	br := bufio.NewReader(tc)

	// POST a receive-pack to an OUT-OF-SCOPE repo, with Expect: 100-continue,
	// and DO NOT send the body yet — we want to observe what the proxy says
	// before any upload.
	fmt.Fprintf(tc, "POST /other/repo.git/git-receive-pack HTTP/1.1\r\nHost: github.com\r\nContent-Length: 1500000\r\nExpect: 100-continue\r\n\r\n")

	tc.SetReadDeadline(time.Now().Add(3 * time.Second))
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	// The FIRST thing back must be the 403 — never "100 Continue".
	if strings.Contains(statusLine, "100") {
		t.Fatalf("proxy sent 100 Continue to an out-of-scope push; body would upload. Got %q", statusLine)
	}
	if !strings.Contains(statusLine, "403") {
		t.Fatalf("first response line = %q, want 403", statusLine)
	}
	if h.gh.count() != 0 {
		t.Errorf("out-of-scope push reached upstream")
	}
}

// rawTLS opens a raw keep-alive TLS conn to the proxy (bypassing http.Client's
// connection pooling) so tests can drive multiple requests on ONE connection.
func (h *harness) rawTLS(t *testing.T, sni string) *tls.Conn {
	t.Helper()
	raw, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(raw, &tls.Config{ServerName: sni, RootCAs: h.caPool, NextProtos: []string{"http/1.1"}})
	if err := tc.Handshake(); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	return tc
}

// TestSequentialKeepAlive drives several requests over one raw TLS connection
// and asserts each response parses (the keep-alive loop stays in sync).
func TestSequentialKeepAlive(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	tc := h.rawTLS(t, "api.github.com")
	defer tc.Close()
	br := bufio.NewReader(tc)
	for i := 0; i < 4; i++ {
		if _, err := io.WriteString(tc, "GET /repos/o/r HTTP/1.1\r\nHost: api.github.com\r\n\r\n"); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("response %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("response %d status = %d", i, resp.StatusCode)
		}
	}
	if h.gh.count() != 4 {
		t.Errorf("upstream saw %d requests, want 4", h.gh.count())
	}
}

// TestUpstreamRejectsBodyClosesConn is the F1 guard: upstream responds to a
// large POST WITHOUT reading its body (GitHub 403'ing an unauthorized push).
// The proxy must NOT reuse the connection with undrained body bytes still in
// the shared reader — that both data-races net/http's background body drain
// and desyncs the next request. Assert: no race (via -race), a first response
// arrives (no hang), and the connection is then closed (the F1 close-on-
// unconsumed-body path fired). The upstream hijacks + closes without reading so
// the proxy's inbound body is deterministically left unconsumed.
func TestUpstreamRejectsBodyClosesConn(t *testing.T) {
	h := newHarness(t, harnessOpts{
		respond: func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			c, buf, err := hj.Hijack()
			if err != nil {
				return
			}
			// Respond WITHOUT reading r.Body, then close the upstream conn.
			buf.WriteString("HTTP/1.1 403 Forbidden\r\nContent-Length: 6\r\nConnection: close\r\n\r\ndenied")
			buf.Flush()
			c.Close()
		},
	})
	tc := h.rawTLS(t, "github.com")
	defer tc.Close()
	br := bufio.NewReader(tc)

	payload := bytes.Repeat([]byte("z"), 1500*1024) // >1MiB, won't all flush before upstream closes
	fmt.Fprintf(tc, "POST /o/r.git/git-receive-pack HTTP/1.1\r\nHost: github.com\r\nContent-Length: %d\r\n\r\n", len(payload))
	go tc.Write(payload) // may block/err as upstream closes early; don't gate the test on it

	tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("no first response (proxy hung or desynced): %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("first status = %d, want 403 (relayed) or 502 (upstream error)", resp.StatusCode)
	}
	// The connection must be closed now (F1): a further read hits EOF/err.
	tc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Errorf("connection still readable after an unconsumed-body upstream reject; F1 close path did not fire")
	}
}

func TestGraphQLBodyTooLarge413(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.httpClient(false)
	big := `{"query":"` + strings.Repeat("a", (1<<20)+100) + `"}`
	req, _ := http.NewRequest("POST", "https://api.github.com/graphql", strings.NewReader(big))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if h.gh.count() != 0 {
		t.Errorf("oversized graphql reached upstream")
	}
}

func TestConnectionCloseNotReused(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	tc := h.rawTLS(t, "api.github.com")
	defer tc.Close()
	br := bufio.NewReader(tc)
	// Client asks to close after this request; the proxy must honor it and not
	// keep the connection alive.
	io.WriteString(tc, "GET /repos/o/r HTTP/1.1\r\nHost: api.github.com\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// Connection header must NOT have been forwarded upstream (hop-by-hop).
	// A follow-up read should hit EOF promptly (server closed).
	tc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Errorf("connection still open after Connection: close")
	}
}

func TestAuditLogContentNoToken(t *testing.T) {
	var buf syncBuffer
	h := newHarnessWithAudit(t, harnessOpts{repos: []string{"allowed/repo"}}, &buf)
	c := h.httpClient(false)

	// Inject path (in scope).
	doGET(t, c, "https://api.github.com/repos/allowed/repo/pulls")
	// Refused path (out of scope).
	resp, _ := doGET(t, c, "https://github.com/other/repo.git/info/refs?service=git-upload-pack")
	resp.Body.Close()

	log := buf.String()
	if !strings.Contains(log, "decision=inject") {
		t.Errorf("audit log missing inject decision line:\n%s", log)
	}
	if !strings.Contains(log, "decision=refused-scope") {
		t.Errorf("audit log missing refused-scope decision line:\n%s", log)
	}
	if strings.Contains(log, h.readTok) || strings.Contains(log, h.writeTok) {
		t.Errorf("audit log leaked a token:\n%s", log)
	}
}

// TestGraphQLExpect100Continue guards the deadlock: a graphql POST that waits
// for 100 Continue before sending its body must NOT hang in the classifier's
// body buffering — 100 must be answered before any body read.
func TestGraphQLExpect100Continue(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.httpClient(false)
	q := `{"query":"mutation { addStar(input:{starrableId:\"x\"}) { clientMutationId } }"}`
	req, _ := http.NewRequest("POST", "https://api.github.com/graphql", strings.NewReader(q))
	req.Header.Set("Expect", "100-continue")
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := c.Do(req)
		if err != nil {
			t.Errorf("graphql expect-continue: %v", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("status = %d", resp.StatusCode)
		}
		if h.gh.last().BodyN != len(q) {
			t.Errorf("graphql body bytes = %d, want %d", h.gh.last().BodyN, len(q))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("graphql Expect:100-continue deadlocked (body read before 100 reply)")
	}
}
