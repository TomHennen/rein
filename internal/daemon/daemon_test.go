package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// startDaemon launches a daemon on socketPath in a goroutine and returns a
// stop func. It waits until the socket is dialable so callers race-free.
func startDaemon(t *testing.T, socketPath string) (stop func()) {
	t.Helper()
	d := New(socketPath, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- d.Serve(ctx) }()

	waitDialable(t, socketPath)
	return func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("Serve returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve did not return after ctx cancel")
		}
	}
}

func waitDialable(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %s never became dialable", socketPath)
}

// ping sends a request and decodes one response line.
func ping(t *testing.T, socketPath string, req Request) Response {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	line, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("no response line: %v", sc.Err())
	}
	var resp Response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response %q: %v", sc.Bytes(), err)
	}
	return resp
}

func TestPingRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), SocketName)
	stop := startDaemon(t, sock)
	defer stop()

	resp := ping(t, sock, Request{Method: "ping"})
	if !resp.OK || !resp.Pong {
		t.Fatalf("ping: got %+v, want {OK:true Pong:true}", resp)
	}
}

func TestUnknownMethod(t *testing.T) {
	sock := filepath.Join(t.TempDir(), SocketName)
	stop := startDaemon(t, sock)
	defer stop()

	resp := ping(t, sock, Request{Method: "no-such-method"})
	if resp.OK || resp.Error == "" {
		t.Fatalf("unknown method: got %+v, want OK:false with error", resp)
	}
}

func TestMalformedRequest(t *testing.T) {
	sock := filepath.Join(t.TempDir(), SocketName)
	stop := startDaemon(t, sock)
	defer stop()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("{not json}\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("no response to malformed line: %v", sc.Err())
	}
	var resp Response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.OK || resp.Error == "" {
		t.Fatalf("malformed request: got %+v, want OK:false with error", resp)
	}
}

func TestSingleInstanceRefuses(t *testing.T) {
	sock := filepath.Join(t.TempDir(), SocketName)
	stop := startDaemon(t, sock)
	defer stop()

	// A second daemon on the same live socket must refuse to start.
	d2 := New(sock, nil)
	err := d2.Serve(context.Background())
	if err == nil {
		d2.Close()
		t.Fatal("second Serve on live socket succeeded; want refusal")
	}
}

func TestRefusedSecondInstanceCloseLeavesLiveSocket(t *testing.T) {
	// A second daemon that refuses to start (single-instance) must NOT, on
	// Close, unlink the live socket owned by the first daemon.
	sock := filepath.Join(t.TempDir(), SocketName)
	stop := startDaemon(t, sock)
	defer stop()

	d2 := New(sock, nil)
	if err := d2.Serve(context.Background()); err == nil {
		d2.Close()
		t.Fatal("second Serve succeeded; want refusal")
	}
	// d2 never bound; its Close must be a no-op for the socket file.
	if err := d2.Close(); err != nil {
		t.Fatalf("refused daemon Close: %v", err)
	}

	// The first daemon must still be live and serving.
	resp := ping(t, sock, Request{Method: "ping"})
	if !resp.OK || !resp.Pong {
		t.Fatalf("first daemon not alive after d2.Close: %+v", resp)
	}
}

func TestStaleSocketCleanup(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, SocketName)

	// Create a stale socket file: a real listener, then closed WITHOUT
	// unlinking, leaving a path with no live listener behind it. Closing a
	// UnixListener unlinks, so instead bind, then close the underlying fd via
	// a fresh listener we abandon. Simplest: listen, grab the file, close, and
	// re-create the path as a leftover socket node.
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	// Prevent Close from unlinking so the file persists as a stale node.
	l.SetUnlinkOnClose(false)
	l.Close()

	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket should still exist: %v", err)
	}
	// Dialing it now should give ECONNREFUSED (no listener).
	if _, derr := net.Dial("unix", sock); !errors.Is(derr, syscall.ECONNREFUSED) {
		t.Fatalf("stale socket dial: got %v, want ECONNREFUSED", derr)
	}

	// The daemon should detect the stale node, remove it, and start.
	stop := startDaemon(t, sock)
	defer stop()
	resp := ping(t, sock, Request{Method: "ping"})
	if !resp.OK || !resp.Pong {
		t.Fatalf("ping after stale cleanup: got %+v", resp)
	}
}

func TestSocketAndDirPerms(t *testing.T) {
	dir := t.TempDir()
	// Deliberately loosen the temp dir so we prove listen() tightens it.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	sock := filepath.Join(dir, SocketName)
	stop := startDaemon(t, sock)
	defer stop()

	dfi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dfi.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}

	sfi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := sfi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket perm = %o, want 600", perm)
	}
}

func TestPeerUIDAcceptsSelf(t *testing.T) {
	// The peer-uid check must NOT reject our own connections (same uid).
	// Cross-uid rejection can't be unit-tested without a second uid; this
	// confirms the check exists and passes for the common case.
	sock := filepath.Join(t.TempDir(), SocketName)
	stop := startDaemon(t, sock)
	defer stop()

	resp := ping(t, sock, Request{Method: "ping"})
	if !resp.OK {
		t.Fatalf("same-uid ping rejected: %+v", resp)
	}
}

func TestCloseIdempotent(t *testing.T) {
	sock := filepath.Join(t.TempDir(), SocketName)
	d := New(sock, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Serve(ctx)
	waitDialable(t, sock)

	if err := d.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file should be gone after Close, stat err = %v", err)
	}
}
