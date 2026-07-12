// Package ghsession encapsulates the "fetch a currently-valid gh-session
// token" logic shared by cmd/rein-gh (the gh shim) and the `rein gh-auth`
// subcommand. Both call EnsureFresh against the same on-disk cache file
// so a token minted by one is reused by the other — no double minting,
// no diverging state.
//
// Phase 0 only. Phase 1's broker daemon holds the gh session token in
// memory and this package collapses into it.
package ghsession

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/TomHennen/rein/internal/runscope"
	"github.com/TomHennen/rein/internal/tokencache"
)

// MintFunc mints an installation token. Callers pass a specific tier:
// in CP3.7 EnsureFresh is used with githubapp.Client.MintGhReadOnlyToken
// (the long-lived cacheable read tier). Write-tier minting in the shim
// is done outside this package because writes are never cached.
type MintFunc func(ctx context.Context) (token string, expiresAt time.Time, err error)

// RevokeFunc revokes an installation token server-side. Used for the
// best-effort revoke of a previously-cached token after a fresh mint.
// Callers typically pass *githubapp.Client.RevokeToken. A nil revoke is
// allowed — the previous token will simply expire on GitHub's ~1h floor.
type RevokeFunc func(ctx context.Context, token string) error

// EnsureFresh returns a currently-valid gh-session token from the cache
// at cachePath. If the cached token is missing or within refreshSkew of
// expiry, mints a fresh one via mintFn, atomically writes it to
// cachePath, and (if revoke is non-nil) best-effort revokes the
// previously-cached token so its effective lifetime tracks usage rather
// than GitHub's ~1h floor.
//
// mintTimeout caps each network call. Cache-hit short-circuits before
// any API call.
func EnsureFresh(
	cachePath string,
	mintFn MintFunc,
	revoke RevokeFunc,
	refreshSkew time.Duration,
	mintTimeout time.Duration,
	logger *log.Logger,
) (token string, expiresAt time.Time, err error) {
	cur, readErr := tokencache.Read(cachePath)
	hasCur := readErr == nil
	if !hasCur && !errors.Is(readErr, os.ErrNotExist) {
		logger.Printf("cache load failed (will mint fresh): %v", readErr)
	}
	if hasCur && cur.Valid(refreshSkew) {
		logger.Printf("cache hit: expires_at=%s ttl=%s",
			cur.ExpiresAt.Format(time.RFC3339),
			time.Until(cur.ExpiresAt).Round(time.Second))
		return cur.Token, cur.ExpiresAt, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), mintTimeout)
	defer cancel()
	token, expiresAt, err = mintFn(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint: %w", err)
	}
	logger.Printf("mint succeeded: expires_at=%s ttl=%s token_len=%d",
		expiresAt.Format(time.RFC3339),
		time.Until(expiresAt).Round(time.Second),
		len(token))

	if err := tokencache.Write(cachePath, tokencache.Entry{Token: token, ExpiresAt: expiresAt}); err != nil {
		logger.Printf("cache write failed (continuing): %v", err)
	}

	// Best-effort revoke of the previous token. Done after we have the
	// new token in hand so a revoke failure can't leave the caller with
	// neither. A nil revoke is the documented opt-out (e.g. tests).
	if revoke != nil && hasCur && cur.Token != "" && cur.Token != token {
		revokeCtx, revokeCancel := context.WithTimeout(context.Background(), mintTimeout)
		if revokeErr := revoke(revokeCtx, cur.Token); revokeErr != nil {
			logger.Printf("revoke of previous token failed (best-effort): %v", revokeErr)
		} else {
			logger.Printf("revoked previous gh-session token")
		}
		// Explicit, not defer: the mint context is still live until
		// function return; freeing this one promptly keeps the two
		// contexts from stacking until the outer return.
		revokeCancel()
	}

	return token, expiresAt, nil
}

// ReadCachePathForScope returns the path within stateDir for the gh
// read-tier token cache of ONE scope ceiling. (The write tier is never
// cached.) The filename carries a fingerprint of the ceiling's scope key
// (runscope.Resolver.Key), so a token minted under a NARROWER earlier run —
// e.g. a single-repo-A run within the ~1h token TTL — is stored under a
// different filename than a wider [A,B] run's token and can never be served
// to it (issue #95). This mirrors the direct-mode git-read cache
// (read-token-<tag>.json) exactly; both derive the tag from
// runscope.CacheTag so the two never diverge.
//
// scopeKey MUST be a runscope.Resolver.Key() value for the CURRENT effective
// ceiling — never a launch-time snapshot — or a still-fresh token could be
// served for a repo it does not cover (a 404/403 inside the agent).
func ReadCachePathForScope(stateDir, scopeKey string) string {
	return filepath.Join(stateDir, "cache", "gh-read-token-"+runscope.CacheTag(scopeKey)+".json")
}

// ReadCacheGlob returns a glob that matches EVERY per-scope gh read-token
// cache file under stateDir, including any legacy untagged
// gh-read-token.json left by an older rein. Diagnostic/cleanup callers
// (`rein doctor`, remediation) enumerate this so they still inspect and
// groom all caches now that the filename varies by scope.
func ReadCacheGlob(stateDir string) string {
	return filepath.Join(stateDir, "cache", "gh-read-token*.json")
}
