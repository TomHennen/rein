package appsetup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBindLoopback_AddressIsLoopback(t *testing.T) {
	ln, port, err := bindLoopback(0)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("listener bound to %q, want 127.0.0.1:*", addr)
	}
	if port == 0 {
		t.Error("ephemeral port not assigned")
	}
}

func TestNewStateNonce_LengthAndUniqueness(t *testing.T) {
	a, err := newStateNonce()
	if err != nil {
		t.Fatalf("nonce a: %v", err)
	}
	b, err := newStateNonce()
	if err != nil {
		t.Fatalf("nonce b: %v", err)
	}
	if a == b {
		t.Errorf("nonces equal: %q == %q", a, b)
	}
	// base64.RawURLEncoding of 32 bytes is 43 chars (no padding).
	if len(a) != 43 {
		t.Errorf("nonce length = %d, want 43", len(a))
	}
}

func newTestManifest(t *testing.T) Manifest {
	t.Helper()
	m, err := BuildManifest(RolePrimary, 12345)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	return m
}

func TestRunCallback_HappyPath(t *testing.T) {
	ln, port, err := bindLoopback(0)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	state := "test-state-xyz"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resCh := make(chan callbackResult, 1)
	errCh := make(chan error, 1)
	go func() {
		r, err := runCallback(ctx, ln, newTestManifest(t), state, RolePrimary, 1, io.Discard)
		if err != nil {
			errCh <- err
			return
		}
		resCh <- r
	}()

	// Wait briefly for server to start serving.
	hammer(t, port, "/", 200)
	// Fire the callback.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?code=abc&state=%s", port, state))
	if err != nil {
		t.Fatalf("callback get: %v", err)
	}
	resp.Body.Close()

	select {
	case r := <-resCh:
		if r.Code != "abc" {
			t.Errorf("code = %q, want abc", r.Code)
		}
	case err := <-errCh:
		t.Fatalf("runCallback err: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("runCallback did not return")
	}
}

func TestRunCallback_StateMismatch(t *testing.T) {
	ln, port, _ := bindLoopback(0)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := runCallback(ctx, ln, newTestManifest(t), "good-state", RolePrimary, 1, io.Discard)
		errCh <- err
	}()
	hammer(t, port, "/", 200)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?code=abc&state=bad-state", port))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected timeout error after state mismatch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runCallback did not return after timeout")
	}
}

func TestRunCallback_RootServesHTML(t *testing.T) {
	ln, port, _ := bindLoopback(0)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := runCallback(ctx, ln, newTestManifest(t), "state", RolePrimary, 1, io.Discard)
		errCh <- err
	}()

	body := getBody(t, port, "/")
	if !bytes.Contains(body, []byte("rein-primary-")) {
		t.Errorf("landing page does not include primary app name: %s", body)
	}
	if !bytes.Contains(body, []byte("github.com/settings/apps/new")) {
		t.Errorf("landing page does not include GitHub create endpoint")
	}

	<-errCh
}

func TestRunCallback_ContextTimeout(t *testing.T) {
	ln, _, _ := bindLoopback(0)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := runCallback(ctx, ln, newTestManifest(t), "state", RolePrimary, 1, io.Discard)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "callback wait") {
		t.Errorf("error %q should mention callback wait", err.Error())
	}
}

func TestRunCallback_SingleShot(t *testing.T) {
	ln, port, _ := bindLoopback(0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resCh := make(chan callbackResult, 1)
	errCh := make(chan error, 1)
	go func() {
		r, err := runCallback(ctx, ln, newTestManifest(t), "state", RolePrimary, 1, io.Discard)
		if err != nil {
			errCh <- err
			return
		}
		resCh <- r
	}()
	hammer(t, port, "/", 200)

	// First call succeeds.
	resp, _ := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?code=first&state=state", port))
	if resp == nil {
		t.Fatal("first callback nil response")
	}
	resp.Body.Close()
	select {
	case r := <-resCh:
		if r.Code != "first" {
			t.Errorf("first code = %q", r.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first callback did not complete")
	}
	// Second call would race with shutdown; even if it lands it
	// must not panic. We don't assert response status because
	// shutdown may have closed the listener already.
	resp2, _ := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?code=second&state=state", port))
	if resp2 != nil {
		resp2.Body.Close()
	}
}

// hammer waits until the server answers a GET / so subsequent test
// requests don't race start-up. Polls for up to 1s.
func hammer(t *testing.T, port int, path string, wantStatus int) {
	t.Helper()
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == wantStatus {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not start in time on port %d", port)
}

func getBody(t *testing.T, port int, path string) []byte {
	t.Helper()
	hammer(t, port, path, 200)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

func TestBindLoopback_NonLocalhostLiteral(t *testing.T) {
	// Ensure bindLoopback uses the IP literal, not "localhost", per
	// RFC 8252 §7.3. We test this by checking that the listener's
	// network is IPv4.
	ln, _, err := bindLoopback(0)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer ln.Close()
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("not a TCP listener: %T", ln.Addr())
	}
	if !tcpAddr.IP.IsLoopback() {
		t.Errorf("listener IP %s is not loopback", tcpAddr.IP)
	}
	if tcpAddr.IP.String() != "127.0.0.1" {
		t.Errorf("listener IP = %s, want 127.0.0.1 literal", tcpAddr.IP)
	}
}
