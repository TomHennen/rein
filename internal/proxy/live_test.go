package proxy

// live_test.go is the CP2 success gate: it proves the productized proxy arm
// (internal/proxy + the real githubapp mints) works against REAL github.com
// with a REAL minted GitHub App token, driving traffic through the per-run
// proxy socket exactly as a sandbox client would.
//
// It is GATED behind REIN_LIVE=1 (t.Skip otherwise) so `go test ./...` stays
// hermetic and never touches the network or the App key. Run it explicitly:
//
//	source ./dev-env
//	REIN_LIVE=1 go test ./internal/proxy -run Live -v
//
// Hard constraint #1: it touches ONLY the throwaway repo REIN_TEST_REPO_A
// (TomHennen/agentcreds-validation-a). It performs NO writes to the repo — the
// write-tier assertion is a git-receive-pack ADVERTISEMENT (info/refs), which
// proves the write token carries push permission without mutating anything.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/brokercore"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/session"
)

// liveHarness wires a real-upstream proxy over a unix socket and records every
// token the proxy mints, so the leak check can grep client-visible bytes for
// the actual injected token strings.
type liveHarness struct {
	socket string
	caPool *x509.CertPool

	client     *githubapp.Client
	mu         sync.Mutex
	minted     []string // every token string the proxy minted (read or write)
	readCalls  int
	writeCalls int
	approvals  int
}

func (h *liveHarness) record(tok string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.minted = append(h.minted, tok)
}

func newLiveHarness(t *testing.T) *liveHarness {
	t.Helper()

	appCfg, ks, err := config.LoadAppConfig()
	if err != nil {
		t.Fatalf("LoadAppConfig (did you source ./dev-env?): %v", err)
	}
	client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
	if err != nil {
		t.Fatalf("githubapp.NewClient: %v", err)
	}

	h := &liveHarness{client: client}

	// Best-effort: revoke every token the proxy minted when the test ends, so a
	// live ~1h contents:write token doesn't linger. A failed revoke is not a
	// problem (the token still expires natively); log and move on.
	t.Cleanup(func() {
		h.mu.Lock()
		toks := append([]string(nil), h.minted...)
		h.mu.Unlock()
		for _, tok := range toks {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := client.RevokeToken(ctx, tok); err != nil {
				t.Logf("cleanup: revoke minted token failed (expires natively): %v", err)
			}
			cancel()
		}
	})

	// Wrap the REAL mints so we (a) count calls and (b) record every token the
	// proxy actually injected, for the leak check. A 5s ctx per mint keeps a
	// clock-skew / network failure from hanging the test.
	mintRead := brokercore.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		h.mu.Lock()
		h.readCalls++
		h.mu.Unlock()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		tok, exp, err := client.MintReadOnlyToken(ctx)
		if err == nil {
			h.record(tok)
		}
		return tok, exp, err
	})
	mintWrite := brokercore.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		h.mu.Lock()
		h.writeCalls++
		h.mu.Unlock()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		tok, exp, err := client.MintWriteToken(ctx)
		if err == nil {
			h.record(tok)
		}
		return tok, exp, err
	})

	// Scope ceiling: the FULL slug via session.Session.Contains, which
	// normalizes case and a trailing ".git" (advisor point 2). A bare-name or
	// exact-match set would refuse the in-scope requests locally and mask real
	// failures behind a false "scope working."
	sess := &session.Session{Repos: []string{os.Getenv("REIN_TEST_REPO_A")}}
	inScope := func(repo string) bool { return sess.Contains(repo) }

	// Non-interactive test: auto-approve writes without a tty prompt.
	approve := func(repo string) bool {
		h.mu.Lock()
		h.approvals++
		h.mu.Unlock()
		return true
	}

	// Keystore-backed CA in a temp dir (constraint #6 path).
	caKS := keystore.NewFileKeystore(t.TempDir())
	ca, err := LoadOrCreateCA(caKS)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	h.caPool = x509.NewCertPool()
	if !h.caPool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("append CA cert to pool failed")
	}

	core := NewSessionCore(SessionConfig{
		SessionID:      "sess_cp2_live",
		MintRead:       mintRead,
		MintWrite:      mintWrite,
		InScope:        inScope,
		EmptyPathScope: "allow",
		Approve:        approve,
		ReadCache:      NewMemCache(),
		Logger:         testLogger(t),
	})

	p, err := New(Config{
		SessionID: "sess_cp2_live",
		Core:      core,
		CA:        ca,
		Logger:    testLogger(t),
		// Upstream nil ⇒ the real CP1-recipe transport against real github.com.
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	h.socket = filepath.Join(t.TempDir(), "proxy.sock")
	ln, err := Listen(h.socket, nil)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Serve(ctx, ln)
	return h
}

// dialProxyTLS opens a TLS conn to the proxy over its unix socket, impersonating
// an in-sandbox client with the given SNI. When useConnect is set it first sends
// the srt CONNECT preamble (the exact path a real sandbox client uses).
func (h *liveHarness) dialProxyTLS(sni string, useConnect bool) (*tls.Conn, error) {
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

// httpClient reaches the proxy over the unix socket, deriving SNI from the URL
// host. Redirects are surfaced (not followed).
func (h *liveHarness) httpClient(useConnect bool) *http.Client {
	return &http.Client{
		Timeout:       30 * time.Second,
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
				return h.dialProxyTLS(host, useConnect)
			},
		},
	}
}

// assertNoTokenLeak scans the client-visible response (status line + headers +
// the already-read body) and fails if any token the proxy minted appears in it.
// body is passed in already-read so this is safe to call after the body was
// consumed/closed; the header dump (DumpResponse with body=false) does not touch
// resp.Body.
func (h *liveHarness) assertNoTokenLeak(t *testing.T, label string, resp *http.Response, body string) {
	t.Helper()
	head, err := httputil.DumpResponse(resp, false)
	if err != nil {
		t.Fatalf("%s: DumpResponse(headers): %v", label, err)
	}
	hay := string(head) + body
	h.mu.Lock()
	toks := append([]string(nil), h.minted...)
	h.mu.Unlock()
	if len(toks) == 0 {
		t.Fatalf("%s: leak check has no minted tokens to scan for (mint never ran?)", label)
	}
	for _, tok := range toks {
		if tok != "" && strings.Contains(hay, tok) {
			t.Errorf("%s: LEAK — minted token appears in client-visible response", label)
		}
	}
}

func TestLiveCP2SuccessGate(t *testing.T) {
	if os.Getenv("REIN_LIVE") != "1" {
		t.Skip("live test; set REIN_LIVE=1 (and source ./dev-env) to run against real github.com")
	}

	slug := os.Getenv("REIN_TEST_REPO_A")
	if slug == "" {
		t.Fatal("REIN_TEST_REPO_A unset; source ./dev-env")
	}
	h := newLiveHarness(t)

	// Report table.
	type row struct {
		name   string
		status string
		pass   bool
	}
	var results []row
	report := func(name string, pass bool, status string) {
		results = append(results, row{name, status, pass})
		verdict := "PASS"
		if !pass {
			verdict = "FAIL"
		}
		t.Logf("[%s] %-28s status=%s", verdict, name, status)
	}

	// --- Criterion 1: READ REST injection (Bearer), CONNECT preamble path ---
	// Use the srt CONNECT preamble here to exercise the exact sandbox path.
	{
		c := h.httpClient(true)
		req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+slug, nil)
		// The CLIENT sends NO Authorization — the proxy injects it.
		if req.Header.Get("Authorization") != "" {
			t.Fatal("test bug: client set its own Authorization")
		}
		resp, err := c.Do(req)
		if err != nil {
			report("1-read-rest", false, "err:"+err.Error())
		} else {
			rl := resp.Header.Get("X-RateLimit-Limit")
			body := readBody(resp)
			h.assertNoTokenLeak(t, "1-read-rest", resp, body)
			// Private repo: anon → 404. 200 proves the injected token authed.
			// X-RateLimit-Limit corroborates (anon=60, installation token 5000+).
			pass := resp.StatusCode == 200
			report("1-read-rest", pass, fmt.Sprintf("%d (ratelimit=%s)", resp.StatusCode, rl))
			if rl != "" {
				if n, e := strconv.Atoi(rl); e == nil && n <= 60 {
					t.Errorf("1-read-rest: ratelimit=%d looks anonymous, token may not have injected", n)
				}
			}
		}
	}

	// --- Criterion 2: READ git transport (Basic x-access-token) ---
	{
		c := h.httpClient(false)
		u := "https://github.com/" + slug + ".git/info/refs?service=git-upload-pack"
		resp, err := c.Do(mustGET(u))
		if err != nil {
			report("2-read-git", false, "err:"+err.Error())
		} else {
			body := readBody(resp)
			h.assertNoTokenLeak(t, "2-read-git", resp, body)
			// Private repo: anon git-upload-pack → 401. 200 proves Basic inject.
			report("2-read-git", resp.StatusCode == 200, strconv.Itoa(resp.StatusCode))
		}
	}

	// --- Criterion 3: WRITE git tier (receive-pack advertisement) ---
	// GET info/refs?service=git-receive-pack is classified WRITE: it exercises
	// the write classifier + write-token mint + approval + Basic injection, and
	// a 200 proves the write token actually carries push permission — WITHOUT
	// mutating the repo (it's only the ref advertisement).
	{
		c := h.httpClient(false)
		u := "https://github.com/" + slug + ".git/info/refs?service=git-receive-pack"
		resp, body := doGET(t, c, u)
		h.assertNoTokenLeak(t, "3-write-git", resp, body)
		pass := resp.StatusCode == 200
		report("3-write-git", pass, strconv.Itoa(resp.StatusCode))
		if !pass {
			// A 403 here = the write token lacks push. Surface GitHub's reason.
			t.Errorf("3-write-git: status=%d body=%q — if 403, the App install may lack contents:write on %s", resp.StatusCode, truncate(body, 300), slug)
		}
		h.mu.Lock()
		wc, ap := h.writeCalls, h.approvals
		h.mu.Unlock()
		if wc == 0 {
			t.Errorf("3-write-git: write mint never fired")
		}
		if ap == 0 {
			t.Errorf("3-write-git: approval hook never fired")
		}
	}

	// --- Criterion 4: SCOPE CEILING refused LOCALLY at the proxy ---
	// A repo NOT in session scope must be refused by rein BEFORE egress: a local
	// 403 with a body starting "rein:", regardless of whether the repo exists.
	{
		c := h.httpClient(false)
		u := "https://api.github.com/repos/" + ownerOf(slug) + "/some-nonexistent-out-of-scope-repo"
		resp, body := doGET(t, c, u)
		local403 := resp.StatusCode == http.StatusForbidden && strings.HasPrefix(body, "rein:")
		report("4-scope-ceiling", local403, fmt.Sprintf("%d body=%q", resp.StatusCode, truncate(body, 60)))
		if !local403 {
			t.Errorf("4-scope-ceiling: want local 403 with rein: body, got %d %q", resp.StatusCode, truncate(body, 200))
		}
		// The refusal body must not leak a token either.
		h.assertNoTokenLeak(t, "4-scope-ceiling", resp, body)
	}

	// --- Criterion 6 (best-effort): NEVER-INJECT host routes to passthrough ---
	// Assert via classifyHost that CDN/asset hosts get passthrough (no inject),
	// which is the invariant behind "rein never adds Authorization there."
	{
		neverInject := []string{"objects.githubusercontent.com", "codeload.github.com", "raw.githubusercontent.com"}
		ok := true
		for _, host := range neverInject {
			if classifyHost(host) != classPassthrough {
				ok = false
				t.Errorf("6-never-inject: classifyHost(%q) != passthrough", host)
			}
		}
		report("6-never-inject", ok, "classifyHost→passthrough")
	}

	// --- Criterion 5: token never appears client-side (asserted inline above) ---
	// assertNoTokenLeak ran on every real response; also confirm at least one
	// token was actually minted (else the leak check was vacuous).
	h.mu.Lock()
	nMinted := len(h.minted)
	h.mu.Unlock()
	if nMinted == 0 {
		t.Error("5-no-token-leak: no tokens were minted; the leak assertions were vacuous")
	}
	report("5-no-token-leak", nMinted > 0, fmt.Sprintf("%d tokens minted, none leaked client-side", nMinted))

	// Final summary table in the log.
	t.Log("=== CP2 live gate summary ===")
	for _, r := range results {
		v := "PASS"
		if !r.pass {
			v = "FAIL"
		}
		t.Logf("  %-18s %s  (%s)", r.name, v, r.status)
	}
}

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b)
}

func mustGET(url string) *http.Request {
	req, _ := http.NewRequest("GET", url, nil)
	return req
}

func ownerOf(slug string) string {
	owner, _, _ := strings.Cut(slug, "/")
	return owner
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
