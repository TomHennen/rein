package appsetup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// callbackResult is what runCallback returns on success.
type callbackResult struct {
	Code string
}

// bindLoopback returns a listener bound to 127.0.0.1 on a kernel-
// assigned ephemeral port. The 127.0.0.1 literal is mandated by
// RFC 8252 §7.3 — never use "localhost" (it could resolve to IPv6
// ::1, or be hijacked via /etc/hosts).
func bindLoopback() (net.Listener, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, fmt.Errorf("bind loopback: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return ln, port, nil
}

// newStateNonce returns 32 bytes from crypto/rand, base64-url-encoded
// (no padding). ~43 chars. Never persisted; lives only in the
// closure servicing the listener.
func newStateNonce() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("nonce entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// runCallback serves the auto-post landing page at "/" and waits for
// GitHub's redirect to "/callback?code=...&state=...". Validates state
// with subtle.ConstantTimeCompare; serves a single-shot completion
// page and signals via channel.
//
// The listener is bound by the caller (bindLoopback) so that the port
// is known BEFORE the manifest is built (manifest.redirect_url embeds
// the port). runCallback owns the server lifecycle and closes it on
// return.
//
// ctx times out the wait (caller passes a 10-min deadline). On
// timeout, returns a descriptive error suggesting re-run if the user
// cancelled in the browser.
//
// step is the [step/2] frame used in the landing-page copy.
func runCallback(ctx context.Context, ln net.Listener, m Manifest, state string, role Role, step int, w io.Writer) (callbackResult, error) {
	html, err := renderAutoPostHTML(m, state, role, step)
	if err != nil {
		return callbackResult{}, err
	}

	mux := http.NewServeMux()
	type cbResult struct {
		code string
	}
	resultCh := make(chan cbResult, 1)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			// Silent 404 — don't let favicon.ico or other browser
			// probes register as the callback or as noise.
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(html)
	})

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		gotState := r.URL.Query().Get("state")
		if subtle.ConstantTimeCompare([]byte(gotState), []byte(state)) != 1 {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		select {
		case resultCh <- cbResult{code: code}:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>rein</title><body><h3>You can close this tab.</h3></body>`)
		default:
			// Already handled; reply 200 so the user's browser
			// doesn't show an error if a duplicate fires.
			w.WriteHeader(http.StatusOK)
		}
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	serveErrCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		// http.ErrServerClosed is the normal-shutdown sentinel; any
		// other error is meaningful (e.g. listener was closed
		// externally before Shutdown could run).
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrCh <- err
		}
		close(serveErrCh)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Fprintf(w, "  listening on http://127.0.0.1:%d (open the link your browser was directed to)\n", ln.Addr().(*net.TCPAddr).Port)
	fmt.Fprintln(w, "  waiting for GitHub to call back (10 min timeout)...")

	select {
	case r := <-resultCh:
		return callbackResult{Code: r.code}, nil
	case err, ok := <-serveErrCh:
		if !ok {
			// channel closed without a value — server exited
			// cleanly before we got a callback. Fall through and
			// wait on ctx (which should already be done if the
			// process is shutting down).
			<-ctx.Done()
			return callbackResult{}, fmt.Errorf("callback server exited: %w", ctx.Err())
		}
		return callbackResult{}, fmt.Errorf("callback server: %w", err)
	case <-ctx.Done():
		return callbackResult{}, fmt.Errorf("callback wait: %w (re-run `rein init` if you cancelled in the browser)", ctx.Err())
	}
}

// localURL is the URL the user's browser should open to land on the
// auto-post page. Loopback-IP literal per RFC 8252 §7.3.
func localURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/", port)
}
