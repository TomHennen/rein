// Package daemon is the resident broker daemon for Phase 1 (sandboxed mode).
//
// The daemon owns the single long-lived brokercore.Core and the in-memory
// read-token cache; tokens live only in its memory, never on disk (design
// §6). It listens on a same-uid-only unix control socket where, in later
// checkpoints, the foreground `rein run` process and the injecting proxy
// reach the mint/scope/approval logic. This file is the MECHANICAL skeleton:
// socket + single-instance + peer-uid + lifecycle + a stub control protocol.
// No token, scope, or approval logic lives here yet — those wire in later by
// adding methods to dispatch (protocol.go) and using d.core / d.cache.
//
// Access control on the socket is layered, strongest first:
//
//  1. The 0700 state dir — only our uid can traverse to the socket. This is
//     the real access control.
//  2. The 0600 socket file — defense-in-depth; also closes a brief window if
//     the dir mode is ever wrong.
//  3. A SO_PEERCRED peer-uid check on every accepted connection — belt-and-
//     suspenders, and the layer that survives a future move of the socket to
//     a more permissive directory.
//
// We use a filesystem socket, NOT an abstract-namespace socket: abstract
// sockets have no path on disk and therefore bypass filesystem permissions
// entirely, defeating layers 1 and 2.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/TomHennen/rein/internal/brokercore"
	"github.com/TomHennen/rein/internal/config"
)

// SocketName is the control-socket filename under the state dir.
const SocketName = "daemon.sock"

// DefaultSocketPath is <config.StateDir()>/daemon.sock. cmd/ wiring uses this;
// tests pass an explicit t.TempDir() path to New so they never touch the real
// state dir.
func DefaultSocketPath() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, SocketName), nil
}

// Daemon is the resident broker server. Construct with New, run with Serve,
// stop with Close (or by cancelling Serve's context).
type Daemon struct {
	socketPath string
	core       *brokercore.Core
	cache      *MemReadCache

	mu       sync.Mutex
	listener *net.UnixListener
	ourUID   uint32

	closeOnce sync.Once
}

// New builds a daemon bound to socketPath. The Core carries the mint/scope/
// approval logic (nil is allowed for the skeleton — the ping path needs none);
// the caller wires core.ReadCache to the returned daemon's cache when it wants
// in-memory caching. New does no I/O: the socket is created by Serve.
func New(socketPath string, core *brokercore.Core) *Daemon {
	cache := NewMemReadCache()
	if core != nil && core.ReadCache == nil {
		core.ReadCache = cache
	}
	return &Daemon{
		socketPath: socketPath,
		core:       core,
		cache:      cache,
		ourUID:     uint32(os.Getuid()),
	}
}

// Cache exposes the daemon's in-memory read cache (e.g. for the caller to
// thread into a Core it builds separately).
func (d *Daemon) Cache() *MemReadCache { return d.cache }

// Serve creates the socket and accepts connections until ctx is cancelled or
// Close is called, then returns nil. It refuses to start if another live
// daemon already owns the socket (single-instance) and fails closed on any
// socket-setup error. Serve blocks; run it in its own goroutine if the caller
// needs to do other work.
func (d *Daemon) Serve(ctx context.Context) error {
	if err := d.listen(); err != nil {
		return err
	}

	// Close the listener when ctx is cancelled; the blocked Accept then
	// returns net.ErrClosed, which the loop treats as a clean stop.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			d.Close()
		case <-stop:
		}
	}()

	return d.acceptLoop()
}

// listen prepares the directory, performs the single-instance check, binds the
// socket, and tightens its mode. Called once from Serve.
func (d *Daemon) listen() error {
	dir := filepath.Dir(d.socketPath)
	// StateDir() MkdirAll(0700) won't tighten a pre-existing dir created under
	// a looser umask, so ensure the dir exists AND is 0700 — it is the primary
	// access control for the socket.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("tighten socket dir to 0700: %w", err)
	}

	if err := d.ensureNoLiveInstance(); err != nil {
		return err
	}

	addr := &net.UnixAddr{Name: d.socketPath, Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		// EADDRINUSE here after our cleanup means a racing daemon won the
		// bind; surface it rather than force-unlinking a socket we don't own.
		return fmt.Errorf("listen on %s: %w", d.socketPath, err)
	}
	// net.Listen creates the socket file masked by umask, so it may be more
	// permissive than 0600. Tighten it immediately. There is a sub-millisecond
	// window where it is looser — acceptable, because the 0700 dir is the real
	// gate and the peer-uid check backs it up.
	if err := os.Chmod(d.socketPath, 0o600); err != nil {
		l.Close()
		return fmt.Errorf("tighten socket to 0600: %w", err)
	}

	d.mu.Lock()
	d.listener = l
	d.mu.Unlock()
	return nil
}

// ensureNoLiveInstance enforces single-instance. It dials the socket path
// BEFORE listening (net.Listen on an existing path fails with EADDRINUSE
// regardless of liveness, so liveness must be probed first):
//
//   - dial succeeds        -> a live daemon owns it -> refuse.
//   - file does not exist  -> nothing to clean up -> proceed.
//   - ECONNREFUSED         -> stale socket (no listener) -> unlink, proceed.
//   - any other dial error -> fail closed; do NOT unlink (could be a live
//     socket we merely lack permission to dial, EPERM, etc.).
func (d *Daemon) ensureNoLiveInstance() error {
	conn, err := net.DialTimeout("unix", d.socketPath, 2*time.Second)
	if err == nil {
		conn.Close()
		return fmt.Errorf("a rein daemon is already running on %s", d.socketPath)
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		// Stale socket: a file with no listener behind it. Safe to remove.
		if rmErr := os.Remove(d.socketPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fmt.Errorf("remove stale socket %s: %w", d.socketPath, rmErr)
		}
		return nil
	}
	// Unknown dial failure — fail closed without unlinking.
	return fmt.Errorf("probe existing socket %s: %w", d.socketPath, err)
}

// acceptLoop accepts and serves connections until the listener is closed.
func (d *Daemon) acceptLoop() error {
	d.mu.Lock()
	l := d.listener
	d.mu.Unlock()

	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil // clean stop via Close / ctx cancel.
			}
			// Transient accept error (e.g. EMFILE). Back off briefly so a
			// persistent error doesn't busy-loop the CPU, then retry.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		go d.handleConn(conn)
	}
}

// handleConn rejects non-same-uid peers (defense-in-depth; the dir/socket
// modes already restrict access), then serves newline-delimited JSON requests
// until the client disconnects.
func (d *Daemon) handleConn(conn *net.UnixConn) {
	defer conn.Close()

	uid, err := peerUID(conn)
	if err != nil {
		// Can't verify the peer -> fail closed.
		return
	}
	if uid != d.ourUID {
		// Different uid: drop without serving. This should be unreachable
		// given the 0700 dir, but it is the last line of defense.
		return
	}

	scanner := bufio.NewScanner(conn)
	enc := json.NewEncoder(conn)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(Response{OK: false, Error: "malformed request: " + err.Error()})
			continue
		}
		_ = enc.Encode(d.dispatch(req))
	}
}

// Close stops accepting connections and removes the socket file. Idempotent
// and safe to call concurrently with Serve.
//
// UnixListener.Close is the SOLE unlinker: it removes only the socket it
// itself bound. We deliberately do NOT os.Remove d.socketPath when the
// listener never came up — if Serve refused to start because another live
// daemon owns that path (single-instance), the file is that daemon's, and
// blindly unlinking it would yank a live socket out from under it.
func (d *Daemon) Close() error {
	var err error
	d.closeOnce.Do(func() {
		d.mu.Lock()
		l := d.listener
		d.mu.Unlock()
		if l != nil {
			err = l.Close() // unlinks d.socketPath (the one we bound)
		}
	})
	return err
}
