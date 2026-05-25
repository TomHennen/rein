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

	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/tokencache"
)

// MintFunc mints an installation token. Callers pass a specific tier:
// in CP3.7 EnsureFresh is used with githubapp.Client.MintGhReadOnlyToken
// (the long-lived cacheable read tier). Write-tier minting in the shim
// is done outside this package because writes are never cached.
type MintFunc func(ctx context.Context) (token string, expiresAt time.Time, err error)

// EnsureFresh returns a currently-valid gh-session token from the cache
// at cachePath. If the cached token is missing or within refreshSkew of
// expiry, mints a fresh one via the supplied mintFn, atomically writes
// it to cachePath, and best-effort revokes the previously-cached token
// so its effective lifetime tracks usage rather than GitHub's ~1h floor.
//
// For revoking the previous token we still need a Client — that's
// constructed from appCfg internally. mintTimeout caps each network
// call. Cache-hit short-circuits before any API call.
func EnsureFresh(
	cachePath string,
	appCfg githubapp.Config,
	mintFn MintFunc,
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
	// neither. Needs a Client; construct it lazily.
	if hasCur && cur.Token != "" && cur.Token != token {
		client, err := githubapp.NewClient(appCfg)
		if err != nil {
			logger.Printf("revoke skipped: NewClient failed: %v", err)
		} else {
			revokeCtx, revokeCancel := context.WithTimeout(context.Background(), mintTimeout)
			defer revokeCancel()
			if revokeErr := client.RevokeToken(revokeCtx, cur.Token); revokeErr != nil {
				logger.Printf("revoke of previous token failed (best-effort): %v", revokeErr)
			} else {
				logger.Printf("revoked previous gh-session token")
			}
		}
	}

	return token, expiresAt, nil
}

// ReadCachePath returns the canonical path within stateDir for the gh
// read-tier token cache. (The write tier is never cached.) Defined as a
// function so callers don't hardcode the layout — if it moves, both
// rein-gh and rein gh-auth follow.
func ReadCachePath(stateDir string) string {
	return filepath.Join(stateDir, "cache", "gh-read-token.json")
}
