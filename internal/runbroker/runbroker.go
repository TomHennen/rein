// Package runbroker is the in-process, per-run broker host for sandboxed mode.
//
// Architecture (Tom, 2026-07-04): the v1 spine is IN-PROCESS PER RUN, not a
// resident daemon. Each `rein run` process hosts the broker core + injecting
// proxy itself; there is no daemon on the spine, no control-socket verbs, no
// session-attach protocol (internal/daemon is shelf code for later tracks).
// Security is equivalent either way — the token lives in a same-uid process
// outside the sandbox — and this shape deletes the CP4 approval relay and the
// #12 control-socket invariant.
//
// Start builds the per-run brokercore.Core (with a FRESH in-memory read cache
// and the proxy's write-token/approval memo), loads/creates the CA through the
// keystore, creates the per-run proxy socket (0700 dir / 0600 socket, outside
// the srt bind-mounts — placement-checked), starts the proxy, and returns a
// Host the caller closes on run exit to tear it all down. CP3 wires this into
// `rein run`'s sandboxed path (emit the srt settings pointing at Host.SocketPath
// with Host.CACertPEM in the trust bundle); this package does NOT touch srt.
package runbroker

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TomHennen/rein/internal/brokercore"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/proxy"
)

// Config holds the per-run inputs. The mint/scope/approval brains are injected
// (brokercore DI): the caller (cmd/rein run) wires githubapp mints, the
// session's Contains predicate, and the approval flow; tests stub them.
type Config struct {
	// SessionID tags audit/log lines.
	SessionID string

	// SocketPath is where the per-run proxy socket is created. It MUST sit
	// outside every srt bind-mount (design §5.3); ForbiddenDirs is checked at
	// creation and fails closed.
	SocketPath    string
	ForbiddenDirs []string

	// MintRead / MintWrite mint installation tokens at the session's scope.
	MintRead  brokercore.MintFunc
	MintWrite brokercore.MintFunc

	// InScope is the session's scope ceiling (session.Session.Contains). Nil
	// disables scope enforcement (tests only).
	InScope func(repo string) bool

	// EmptyPathScope governs a request whose repo can't be derived from the
	// path. "" / "allow" (default) or "refuse".
	EmptyPathScope string

	// Approve is the write-approval hook (design §5.5) — the human write
	// control. The proxy memoizes its result per repo for the run. A nil
	// Approve means "no human gate," which is FAIL-OPEN: Start rejects it
	// unless allowAutoApprove is explicitly set. cmd/rein run must always
	// wire a real Approve.
	Approve func(repo string) bool

	// allowAutoApprove opts in to nil-Approve auto-approval. Unexported on
	// purpose: only in-package tests can set it, so a production caller can
	// never silently get an ungated write path by leaving Approve nil.
	allowAutoApprove bool

	// RecordWrite, if set, receives each minted write token (issue #20 ledger).
	RecordWrite func(token string, expiresAt time.Time)

	// CAKeystore is where the proxy CA's cert+key PEM is stored/read (constraint
	// #6). Required.
	CAKeystore keystore.Keystore

	// Audit, if set, receives the token-redacted proxy decision log.
	Audit io.Writer

	// Upstream overrides the transport used to reach GitHub. Nil = the real
	// CP1-recipe transport (HTTP/1.1, system roots). Tests inject a fake.
	Upstream http.RoundTripper

	// IdleTimeout / HardTTL bound the run's live approved-write capability
	// (design §5.3). After IdleTimeout with NO proxy activity, or HardTTL of
	// wall-clock regardless of activity, OnExpire fires and the proxy is torn
	// down (subsequent GitHub requests then fail closed). Zero disables that
	// bound; cmd/rein wires DefaultIdleTimeout / DefaultHardTTL.
	IdleTimeout time.Duration
	HardTTL     time.Duration

	// OnExpire, if set, runs once when idle/hard expiry trips, BEFORE the proxy
	// is stopped — the caller revokes the run's write tokens and prints a loud
	// message here. reason is "idle" or "hard-ttl". It must NOT kill the agent
	// process (the run tears its own credential path down; the agent keeps
	// running credential-less).
	OnExpire func(reason string)

	// checkInterval overrides the expiry poll cadence (tests set it small).
	// Zero derives a sane value from the bounds. Unexported: production never
	// needs to tune it.
	checkInterval time.Duration

	// now is the clock for expiry, injectable for tests. Nil = time.Now.
	now func() time.Time

	// Logger receives forensic lines (never a token). Required.
	Logger *log.Logger
}

// Host is a running per-run broker. Close it on run exit to release the socket
// and stop the proxy.
type Host struct {
	socketPath string
	ca         *proxy.CA
	ln         net.Listener
	cancel     context.CancelFunc
	done       chan struct{}
	closeOnce  sync.Once

	// Expiry state (design §5.3). start is the launch instant (hard-TTL base);
	// lastActivity is the atomic unixnano of the last proxy request (idle base),
	// updated lock-free from the request path via markActivity. expireOnce
	// guards the one-shot revoke+teardown.
	start        time.Time
	lastActivity atomic.Int64
	expireOnce   sync.Once
}

// Start builds the core + proxy, creates the socket, and begins serving. On any
// setup error it cleans up and returns — nothing is left half-started.
func Start(cfg Config) (*Host, error) {
	if cfg.SocketPath == "" {
		return nil, errors.New("runbroker: SocketPath is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("runbroker: Logger is required")
	}
	if cfg.CAKeystore == nil {
		return nil, errors.New("runbroker: CAKeystore is required")
	}
	// Fail closed on a misspelled scope knob: anything but the two known values
	// would otherwise silently mean "allow" (brokercore treats non-"refuse" as
	// allow), fail-open on a typo.
	switch cfg.EmptyPathScope {
	case "", "allow", "refuse":
	default:
		return nil, errors.New(`runbroker: EmptyPathScope must be "", "allow", or "refuse"`)
	}
	// The write-approval hook is the human control (design §5.5); a nil hook
	// fails open. Refuse to start a real run without it. Tests opt in via the
	// unexported allowAutoApprove.
	if cfg.Approve == nil && !cfg.allowAutoApprove {
		return nil, errors.New("runbroker: Approve is required (a nil write-approval hook would auto-approve every write; wire the approval channel)")
	}

	ca, err := proxy.LoadOrCreateCA(cfg.CAKeystore)
	if err != nil {
		return nil, err
	}

	core := proxy.NewSessionCore(proxy.SessionConfig{
		SessionID:      cfg.SessionID,
		MintRead:       cfg.MintRead,
		MintWrite:      cfg.MintWrite,
		InScope:        cfg.InScope,
		EmptyPathScope: cfg.EmptyPathScope,
		Approve:        cfg.Approve,
		ReadCache:      proxy.NewMemCache(), // FRESH per run — never shared across sessions
		RecordWrite:    cfg.RecordWrite,
		Logger:         cfg.Logger,
	})

	var audit *proxy.AuditLog
	if cfg.Audit != nil {
		audit = proxy.NewAuditLog(cfg.Audit)
	}

	now := cfg.now
	if now == nil {
		now = time.Now
	}

	h := &Host{
		socketPath: cfg.SocketPath,
		done:       make(chan struct{}),
		start:      now(),
	}
	h.lastActivity.Store(now().UnixNano()) // count idle from launch, not epoch

	p, err := proxy.New(proxy.Config{
		SessionID: cfg.SessionID,
		Core:      core,
		CA:        ca,
		Audit:     audit,
		Logger:    cfg.Logger,
		Upstream:  cfg.Upstream,
		// Per-request idle signal for the expiry monitor. Cheap atomic store.
		OnActivity: func() { h.markActivity(now()) },
	})
	if err != nil {
		return nil, err
	}

	// Placement check + socket creation (0700 dir, 0600 filesystem socket).
	ln, err := proxy.Listen(cfg.SocketPath, cfg.ForbiddenDirs)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.ca = ca
	h.ln = ln
	h.cancel = cancel
	go func() {
		defer close(h.done)
		_ = p.Serve(ctx, ln)
	}()

	// Expiry monitor: only when a bound is configured. It shares ctx with Serve,
	// so Close (which cancels ctx) also stops the monitor.
	if cfg.IdleTimeout > 0 || cfg.HardTTL > 0 {
		interval := cfg.checkInterval
		if interval <= 0 {
			interval = deriveCheckInterval(cfg.IdleTimeout, cfg.HardTTL)
		}
		go h.monitor(ctx, cfg.IdleTimeout, cfg.HardTTL, interval, now, cfg.OnExpire)
	}
	return h, nil
}

// SocketPath is the per-run proxy socket the sandbox connects to.
func (h *Host) SocketPath() string { return h.socketPath }

// CACertPEM is the CA certificate (no private key) for the sandbox trust
// bundle (CP3: SSL_CERT_FILE = system roots + this).
func (h *Host) CACertPEM() []byte { return h.ca.CertPEM() }

// Close stops the proxy and releases the socket. Idempotent; blocks until the
// serve loop has returned so the socket is fully gone on return.
func (h *Host) Close() error {
	h.closeOnce.Do(func() {
		h.cancel()
		h.ln.Close()
		<-h.done
	})
	return nil
}
