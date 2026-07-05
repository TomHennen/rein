package runbroker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/keystore"
)

// fakeUpstream records the Authorization header the proxy forwarded upstream.
type fakeUpstream struct {
	mu   sync.Mutex
	auth string
	host string
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	f.auth = r.Header.Get("Authorization")
	f.host = r.Host
	f.mu.Unlock()
	io.WriteString(w, "ok")
}

func (f *fakeUpstream) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auth
}

func startHost(t *testing.T, opts Config) (*Host, *fakeUpstream) {
	t.Helper()

	up := &fakeUpstream{}
	srv := httptest.NewUnstartedServer(up)
	srv.EnableHTTP2 = false
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ghURL, _ := url.Parse(srv.URL)

	opts.Upstream = &http.Transport{
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
		TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", ghURL.Host)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	if opts.CAKeystore == nil {
		opts.CAKeystore = keystore.NewFileKeystore(t.TempDir())
	}
	if opts.SocketPath == "" {
		opts.SocketPath = filepath.Join(t.TempDir(), "run", "proxy.sock")
	}
	if opts.MintRead == nil {
		opts.MintRead = func(context.Context) (string, time.Time, error) {
			return "READ-TOK", time.Now().Add(time.Hour), nil
		}
	}
	if opts.MintWrite == nil {
		opts.MintWrite = func(context.Context) (string, time.Time, error) {
			return "WRITE-TOK", time.Now().Add(time.Hour), nil
		}
	}
	if opts.Approve == nil {
		opts.allowAutoApprove = true // test opt-in; production must wire Approve
	}

	h, err := Start(opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h, up
}

// clientThrough builds an http.Client that reaches the host's proxy socket via
// TLS, trusting the host's CA.
func clientThrough(t *testing.T, h *Host) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(h.CACertPEM()) {
		t.Fatal("append CA")
	}
	return &http.Client{
		Transport: &http.Transport{
			ForceAttemptHTTP2: false,
			TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					host = addr
				}
				raw, err := net.Dial("unix", h.SocketPath())
				if err != nil {
					return nil, err
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

func TestHostInjectsEndToEnd(t *testing.T) {
	h, up := startHost(t, Config{SessionID: "s", EmptyPathScope: "allow"})
	c := clientThrough(t, h)

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

func TestHostScopeRefusal(t *testing.T) {
	h, up := startHost(t, Config{
		SessionID: "s",
		InScope:   func(repo string) bool { return strings.HasPrefix(strings.ToLower(repo), "allowed/") },
	})
	c := clientThrough(t, h)
	resp, err := c.Get("https://github.com/other/repo.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if up.lastAuth() != "" {
		t.Errorf("out-of-scope request reached upstream (auth=%q)", up.lastAuth())
	}
}

func TestHostCloseRemovesSocket(t *testing.T) {
	h, _ := startHost(t, Config{SessionID: "s", EmptyPathScope: "allow"})
	sock := h.SocketPath()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket not created: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket still present after Close: stat err = %v", err)
	}
	// Second Close is a no-op.
	if err := h.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestHostCloseTerminatesInflightConn(t *testing.T) {
	h, _ := startHost(t, Config{SessionID: "s", EmptyPathScope: "allow"})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(h.CACertPEM())

	// Open a raw keep-alive TLS conn and complete one request so the handler is
	// live and looping for the next request.
	raw, err := net.Dial("unix", h.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(raw, &tls.Config{ServerName: "api.github.com", RootCAs: pool, NextProtos: []string{"http/1.1"}})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	io.WriteString(tc, "GET /repos/o/r HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read first response: %v", err)
	}
	io.Copy(io.Discard, resp.Body) // drain the body so br holds no leftover bytes
	resp.Body.Close()

	// Close must terminate the in-flight connection (capability must not
	// outlive the run) and must not hang.
	done := make(chan struct{})
	go func() { h.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Host.Close hung with an in-flight connection")
	}

	// The server side must be closed now: a read returns EOF/err promptly.
	tc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Errorf("in-flight connection still readable after Close; capability outlived the run")
	}
}

// TestHostCloseUnderConcurrentTraffic drives many concurrent requests while
// Close races in, asserting no panic/race and that Close returns cleanly (run
// under -race).
func TestHostCloseUnderConcurrentTraffic(t *testing.T) {
	h, _ := startHost(t, Config{SessionID: "s", EmptyPathScope: "allow"})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := clientThrough(t, h)
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := c.Get("https://api.github.com/repos/o/r")
				if err != nil {
					return // expected once Close tears the proxy down
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}

	time.Sleep(50 * time.Millisecond) // let traffic get in flight
	done := make(chan struct{})
	go func() { h.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung under concurrent traffic")
	}
	close(stop)
	wg.Wait()
}

func TestHostPlacementFailsClosed(t *testing.T) {
	bind := t.TempDir()
	_, err := Start(Config{
		SessionID:        "s",
		SocketPath:       filepath.Join(bind, "proxy.sock"),
		ForbiddenDirs:    []string{bind},
		Logger:           log.New(io.Discard, "", 0),
		CAKeystore:       keystore.NewFileKeystore(t.TempDir()),
		allowAutoApprove: true, // reach the placement check, not the Approve gate
	})
	if err == nil {
		t.Fatal("Start allowed a socket inside a forbidden bind-mount")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("placement error = %v, want a forbidden-directory error", err)
	}
}

func TestHostFailsClosedWithoutApprove(t *testing.T) {
	// A real run (no allowAutoApprove) must refuse to start without an Approve
	// hook — a nil hook would auto-approve every write (F9).
	_, err := Start(Config{
		SessionID:  "s",
		SocketPath: filepath.Join(t.TempDir(), "p.sock"),
		Logger:     log.New(io.Discard, "", 0),
		CAKeystore: keystore.NewFileKeystore(t.TempDir()),
	})
	if err == nil {
		t.Fatal("Start succeeded with a nil Approve and no opt-in; want fail-closed")
	}
	if !strings.Contains(err.Error(), "Approve is required") {
		t.Errorf("err = %v, want an Approve-required error", err)
	}
}

func TestHostCACertReusedAcrossRestart(t *testing.T) {
	ks := keystore.NewFileKeystore(t.TempDir())
	h1, _ := startHost(t, Config{SessionID: "s", CAKeystore: ks, EmptyPathScope: "allow"})
	pem1 := append([]byte(nil), h1.CACertPEM()...)
	h1.Close()

	h2, _ := startHost(t, Config{SessionID: "s", CAKeystore: ks, EmptyPathScope: "allow"})
	if !bytes.Equal(pem1, h2.CACertPEM()) {
		t.Errorf("CA cert changed across host restart; not persisted/reused")
	}
}
