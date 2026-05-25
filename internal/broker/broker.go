// Package broker implements the git credential-helper protocol on top of
// a github-app-backed token minter, with a two-tier read/write split per
// design §4.2.5.
//
// The defining invariant is TM-G8 (design §5.3): for any github.com get
// request, the helper MUST exit 0 with a non-empty credential block — never
// empty, never error. An empty/error return triggers downstream agents
// (validated against Claude Code in §12.1) to run `gh auth setup-git`,
// silently rewriting ~/.gitconfig and displacing the broker. The placeholder
// path inside the read and write branches enforces this when the real mint
// fails.
//
// # Two-tier split (CP3)
//
// Each get invocation decides between a cached read token and a freshly-
// minted write token based on a "write intent" signal supplied by the
// caller. Because the git credential-helper protocol asks for credentials
// at the repo-URL level (before deciding fetch vs push), the helper alone
// cannot tell what operation is about to happen. PLAN's original
// /git-receive-pack path inspection turned out not to exist (2026-05-25
// note in PLAN.md), and a pre-push hook fires too late (after refs are
// retrieved, which already requires write-capable creds for a push).
//
// The chosen Shape B mechanism is a PATH shim (`cmd/rein-git`) that sets
// REIN_GIT_OP before exec'ing the real git; the env propagates through to
// the credential helper. The shim is the primary signal. The fallback is
// process-tree introspection (the helper walks /proc to find `git push`
// or `git send-pack` in its ancestor chain). The broker package is
// signal-agnostic — both forms are wrapped in a Config.DetectWrite
// callback the caller (cmd/rein) provides.
//
// This is a routing signal, not a security boundary. Misdetection causes a
// wrong-tier mint, not a security breach — the role's permissions ceiling
// (enforced by GitHub at the token-mint API) remains authoritative.
//
// In Shape A (Phase 1+, sandbox-composed) the proxy inspects actual HTTP
// method/path at the network boundary and supplies the same signal more
// definitively. The broker logic in this package is reused unchanged.
//
// Non-github.com hosts get an empty credential block on purpose — that is
// the credential-helper protocol's "I don't handle this host" signal, and
// the TM-G8 self-remediation concern only applies to the github.com path
// the agent is being prevented from rewriting.
package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MintFunc mints a fresh installation token (read or write, depending on
// which MintFunc this is). Returned as a function (rather than an interface)
// so tests can stub trivially.
type MintFunc func(ctx context.Context) (token string, expiresAt time.Time, err error)

// Config controls credential-helper behavior. Logger and MintRead are
// always required; MintWrite is required only if DetectWrite is set.
type Config struct {
	// MintRead produces a read-only installation token. Used both for direct
	// read serving and to refill the read-token cache.
	MintRead MintFunc

	// MintWrite produces a write installation token. Called when DetectWrite
	// signals true. Never cached.
	MintWrite MintFunc

	// MintTimeout caps each mint attempt. On timeout we fall back to the
	// TM-G8 placeholder for github.com get requests.
	MintTimeout time.Duration

	// Logger receives forensic log lines. The helper must never log raw
	// token values — only metadata (expiry, length, scope).
	Logger *log.Logger

	// ReadCachePath is the file path where the most recent read token is
	// cached as JSON. Empty disables the cache (every read mints fresh).
	// Phase 0 uses a file because each helper invocation is a separate
	// process; Phase 1's daemon will hold the cache in memory.
	ReadCachePath string

	// ReadCacheSkew refreshes the cached read token when it has less than
	// this much time left, to avoid handing out a token that expires in
	// flight. Defaults to 30s if zero.
	ReadCacheSkew time.Duration

	// DetectWrite returns true when this helper invocation is for a write
	// operation. Nil disables write detection (every get serves the cached
	// read path). The implementation is intentionally pluggable: cmd/rein
	// provides one that inspects REIN_GIT_OP (set by the rein-git shim)
	// with a process-tree fallback; tests stub it; Phase 1's proxy provides
	// a variant driven by HTTP method/path inspection.
	//
	// The callback should be fail-closed (return false when the signal is
	// absent or ambiguous): a wrong-tier mint defaulting to read causes a
	// push to surface a 403 — observable and recoverable. The reverse
	// would silently over-grant.
	DetectWrite func() bool

	// Revoke optionally revokes an installation token server-side. When
	// non-nil, the helper calls it on the store/erase actions for any
	// token that doesn't match the cached read token — effectively
	// tightening write-token TTL to "push duration + revoke latency"
	// rather than the GitHub-imposed ~1h. Best-effort: failures are
	// logged and ignored.
	Revoke func(ctx context.Context, token string) error
}

// RunCredentialHelper drives the protocol for one invocation. action is the
// git-supplied subcommand ("get", "store", "erase"). stdin carries the
// attribute block; stdout receives the helper's response.
//
// It returns nil on every well-formed invocation regardless of mint outcome.
// A non-nil error indicates a programming bug (missing config, broken stdin)
// the caller should surface, not a credential-mint failure.
func RunCredentialHelper(action string, stdin io.Reader, stdout io.Writer, cfg Config) error {
	if cfg.Logger == nil {
		return fmt.Errorf("broker: Logger is required")
	}
	if cfg.MintRead == nil {
		return fmt.Errorf("broker: MintRead is required")
	}
	if cfg.DetectWrite != nil && cfg.MintWrite == nil {
		return fmt.Errorf("broker: MintWrite is required when DetectWrite is set")
	}
	cfg.applyDefaults()

	attrs, err := parseAttrs(stdin, cfg.Logger)
	if err != nil {
		// I/O error on stdin (extremely unlikely for a local git invocation).
		// We can't tell whether this was the github.com path, so we can't
		// safely return a TM-G8 placeholder. Returning empty is the lesser
		// evil — a Bearer for the wrong host would also be wrong.
		cfg.Logger.Printf("invocation rejected: stdin read error: %v", err)
		return nil
	}

	host := attrs["host"]
	protocol := attrs["protocol"]
	cfg.Logger.Printf("invoked: action=%q protocol=%q host=%q path=%q",
		action, protocol, host, attrs["path"])

	switch action {
	case "store", "erase":
		return handleStoreErase(attrs, cfg)
	case "get":
		return handleGet(attrs, stdout, cfg)
	default:
		cfg.Logger.Printf("unknown action %q; no-op", action)
		return nil
	}
}

// handleStoreErase opportunistically revokes a write token whose useful
// life is over. Git invokes the helper with store on a successful auth
// and with erase on rejected auth; both fire AFTER the operation, with
// the just-used credential on stdin. The helper compares the presented
// token to the cached read token (if any) — a match means git is
// re-presenting our cached read, which we want to keep alive for the
// session, so we leave it. A non-match means it's a fresh write mint
// whose usefulness is over, and we revoke it to tighten its TTL from
// GitHub's ~1h down to "operation duration + revoke latency".
//
// All work here is best-effort: no Revoke configured, host wrong,
// missing token attribute, or revoke API failure — all silently
// degrade to "leave token alive until its native TTL." The helper
// always returns nil (exit 0); a failed revoke is never a credential
// failure.
func handleStoreErase(attrs map[string]string, cfg Config) error {
	if cfg.Revoke == nil {
		return nil
	}
	if attrs["host"] != "github.com" || attrs["protocol"] != "https" {
		return nil
	}
	token := attrs["password"]
	if token == "" {
		return nil
	}

	// If the presented token is the cached read token, git is just
	// re-presenting what we already gave it for a fetch — keep the
	// cache warm and don't revoke. If the cache file is missing
	// (rare: tmpfs eviction, cache dir manually wiped), we treat the
	// presented token as a write and revoke. The cost of a false
	// positive is a wasted read mint on the next get — TTL annoyance,
	// not a security or correctness issue.
	if cached, ok := readCacheRaw(cfg); ok && cached.Token == token {
		cfg.Logger.Printf("store/erase: token matches cached read; leaving alive")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.MintTimeout)
	defer cancel()
	if err := cfg.Revoke(ctx, token); err != nil {
		cfg.Logger.Printf("store/erase: revoke failed (best-effort): %v", err)
		return nil
	}
	cfg.Logger.Printf("store/erase: revoked token (len=%d) — effective TTL ended at operation completion", len(token))
	return nil
}

// readCacheRaw is readCache without the skew check — used by store/erase
// to identify a cached read token that may already be within the refresh
// window. We don't want to revoke a token that we'd otherwise still hand
// out as a cache hit on the next call.
func readCacheRaw(cfg Config) (cachedToken, bool) {
	if cfg.ReadCachePath == "" {
		return cachedToken{}, false
	}
	body, err := os.ReadFile(cfg.ReadCachePath)
	if err != nil {
		return cachedToken{}, false
	}
	var c cachedToken
	if err := json.Unmarshal(body, &c); err != nil {
		return cachedToken{}, false
	}
	return c, true
}

// applyDefaults fills zero-valued duration fields with the package defaults.
func (c *Config) applyDefaults() {
	if c.ReadCacheSkew == 0 {
		c.ReadCacheSkew = 30 * time.Second
	}
	if c.MintTimeout == 0 {
		// Git users feel this on every mint and every revoke. Tight but
		// not aggressive.
		c.MintTimeout = 5 * time.Second
	}
}

// handleGet is the TM-G8-bearing path. Split out for direct testing.
func handleGet(attrs map[string]string, stdout io.Writer, cfg Config) error {
	host := attrs["host"]
	protocol := attrs["protocol"]

	// Only github.com over HTTPS is in scope. SSH (and any other protocol)
	// uses key-based auth and would just fail with a Bearer token.
	if host != "github.com" || protocol != "https" {
		cfg.Logger.Printf("not handled: protocol=%q host=%q; returning empty", protocol, host)
		return nil
	}

	if isWriteIntent(cfg) {
		return serveWrite(stdout, cfg)
	}
	return serveRead(stdout, cfg)
}

// isWriteIntent invokes the caller-supplied DetectWrite, defaulting to
// false when none is configured. A panic in the callback is recovered and
// treated as "no write intent" — TM-G8 must never be brought down by a
// detector bug.
func isWriteIntent(cfg Config) (write bool) {
	if cfg.DetectWrite == nil {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			cfg.Logger.Printf("DetectWrite panicked: %v; defaulting to read", r)
			write = false
		}
	}()
	write = cfg.DetectWrite()
	if write {
		cfg.Logger.Printf("write intent detected")
	}
	return write
}

// serveWrite mints a fresh write token and writes it to stdout. On mint
// failure it returns the TM-G8 placeholder.
func serveWrite(stdout io.Writer, cfg Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.MintTimeout)
	defer cancel()
	token, expiresAt, err := cfg.MintWrite(ctx)
	if err != nil {
		cfg.Logger.Printf("write mint failed: %v; returning TM-G8 placeholder credential", err)
		return writeCredential(stdout, "x-access-token", "rein-placeholder-mint-failed")
	}
	cfg.Logger.Printf("write mint succeeded: tier=write expires_at=%s ttl=%s token_len=%d",
		expiresAt.Format(time.RFC3339),
		time.Until(expiresAt).Round(time.Second),
		len(token))
	return writeCredential(stdout, "x-access-token", token)
}

// serveRead returns a valid cached read token if present, or mints a fresh
// one and caches it. On mint failure it returns the TM-G8 placeholder.
func serveRead(stdout io.Writer, cfg Config) error {
	if cached, ok := readCache(cfg); ok {
		cfg.Logger.Printf("read cache hit: expires_at=%s ttl=%s token_len=%d",
			cached.ExpiresAt.Format(time.RFC3339),
			time.Until(cached.ExpiresAt).Round(time.Second),
			len(cached.Token))
		return writeCredential(stdout, "x-access-token", cached.Token)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.MintTimeout)
	defer cancel()
	token, expiresAt, err := cfg.MintRead(ctx)
	if err != nil {
		cfg.Logger.Printf("read mint failed: %v; returning TM-G8 placeholder credential", err)
		return writeCredential(stdout, "x-access-token", "rein-placeholder-mint-failed")
	}
	cfg.Logger.Printf("read mint succeeded: tier=read expires_at=%s ttl=%s token_len=%d",
		expiresAt.Format(time.RFC3339),
		time.Until(expiresAt).Round(time.Second),
		len(token))
	writeCache(cfg, cachedToken{Token: token, ExpiresAt: expiresAt})
	return writeCredential(stdout, "x-access-token", token)
}

// cachedToken is the on-disk representation of a cached read token.
type cachedToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// readCache returns the cached read token when present and not within the
// expiry skew. Any error (file missing, corrupt JSON, near expiry) is a
// cache miss; the caller will mint fresh.
func readCache(cfg Config) (cachedToken, bool) {
	if cfg.ReadCachePath == "" {
		return cachedToken{}, false
	}
	body, err := os.ReadFile(cfg.ReadCachePath)
	if err != nil {
		if !os.IsNotExist(err) {
			cfg.Logger.Printf("read cache load failed: %v; will mint fresh", err)
		}
		return cachedToken{}, false
	}
	var c cachedToken
	if err := json.Unmarshal(body, &c); err != nil {
		cfg.Logger.Printf("read cache corrupt: %v; will mint fresh", err)
		return cachedToken{}, false
	}
	if time.Until(c.ExpiresAt) <= cfg.ReadCacheSkew {
		cfg.Logger.Printf("read cache expired or within skew (expires=%s); will mint fresh",
			c.ExpiresAt.Format(time.RFC3339))
		return cachedToken{}, false
	}
	return c, true
}

// writeCache persists a freshly-minted read token. Failures are logged and
// swallowed — caching is best-effort; the operation still succeeds with the
// token already returned to git.
func writeCache(cfg Config, c cachedToken) {
	if cfg.ReadCachePath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cfg.ReadCachePath), 0o700); err != nil {
		cfg.Logger.Printf("read cache mkdir failed: %v; continuing without cache", err)
		return
	}
	body, err := json.Marshal(c)
	if err != nil {
		cfg.Logger.Printf("read cache marshal failed: %v; continuing without cache", err)
		return
	}
	// Write to a tempfile and rename for atomicity — a half-written file
	// would look corrupt to the next reader.
	tmp := cfg.ReadCachePath + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		cfg.Logger.Printf("read cache write failed: %v; continuing without cache", err)
		return
	}
	if err := os.Rename(tmp, cfg.ReadCachePath); err != nil {
		cfg.Logger.Printf("read cache rename failed: %v; continuing without cache", err)
		_ = os.Remove(tmp)
		return
	}
}

// parseAttrs reads git's credential attribute block: one key=value per line,
// terminated by a blank line or EOF. The special "url" attribute (per
// gitcredentials(7)) is parsed and used to backfill protocol/host/path when
// the caller sent only the URL form — some git invocations do, particularly
// when credential.useHttpPath is set.
//
// Malformed lines (no "=") are logged and skipped, not fatal. A future git
// version sending one stray line must not be able to prevent the github.com
// guard inside handleGet from running — that guard is the TM-G8 backstop.
// Only an actual I/O error on r yields a non-nil return.
func parseAttrs(r io.Reader, logger *log.Logger) (map[string]string, error) {
	attrs := make(map[string]string)
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := s.Text()
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			logger.Printf("skipping malformed attribute line %q", line)
			continue
		}
		attrs[k] = v
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if raw, ok := attrs["url"]; ok {
		if u, err := url.Parse(raw); err == nil {
			if u.Scheme != "" && attrs["protocol"] == "" {
				attrs["protocol"] = u.Scheme
			}
			if u.Host != "" && attrs["host"] == "" {
				attrs["host"] = u.Host
			}
			if u.Path != "" && attrs["path"] == "" {
				attrs["path"] = strings.TrimPrefix(u.Path, "/")
			}
		}
	}
	return attrs, nil
}

func writeCredential(w io.Writer, username, password string) error {
	if _, err := fmt.Fprintf(w, "username=%s\npassword=%s\n\n", username, password); err != nil {
		return fmt.Errorf("write credential: %w", err)
	}
	return nil
}
