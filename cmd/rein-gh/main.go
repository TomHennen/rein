// rein-gh is a thin wrapper around the system `gh`. On invocation it
// makes sure a fresh gh-session installation token is available, sets
// GH_TOKEN in the environment, and execs the real gh.
//
// "Fresh" means at least 5 minutes of life remaining (GitHub's
// installation tokens last ~1h; we refresh on stale to absorb the
// agent-session-longer-than-one-hour case that breaks the env-file
// alternative).
//
// When refreshing, the previous token is revoked best-effort
// (DELETE /installation/token) so its effective lifetime tracks usage
// rather than GitHub's ~1h floor.
//
// Installed at the front of $PATH alongside rein-git by `rein
// install-shim`. Misclassification at this layer cannot exceed the
// gh-session token's scope ceiling (single repo, contents:write +
// issues:write + pull_requests:write + metadata:read).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/githubapp"
)

const (
	// refreshSkew refreshes the token when less than this much life
	// remains, so a long-running gh subcommand doesn't time out mid-call.
	refreshSkew = 5 * time.Minute

	// mintTimeout caps the mint API call. gh users feel this latency
	// directly on the first call after expiry; keep it tight.
	mintTimeout = 5 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "rein-gh: %v\n", err)
		os.Exit(127)
	}
}

func run() error {
	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}
	tokenPath := filepath.Join(stateDir, "cache", "gh-token.json")
	logger, closeLog, err := openLog(stateDir)
	if err != nil {
		return err
	}
	defer closeLog()

	token, err := ensureFreshToken(tokenPath, logger)
	if err != nil {
		// Mint failure shouldn't prevent gh from running entirely — fall
		// through to exec gh without GH_TOKEN. gh will either use the
		// user's hosts.yml (if any) or surface its own "needs auth"
		// error, which is more informative than a shim error.
		logger.Printf("token unavailable: %v; execing gh without GH_TOKEN", err)
		token = ""
	}

	realGh, err := findRealGh(os.Args[0])
	if err != nil {
		return err
	}

	env := os.Environ()
	if token != "" {
		env = append(env, "GH_TOKEN="+token)
	}
	argv := append([]string{realGh}, os.Args[1:]...)
	return syscall.Exec(realGh, argv, env)
}

// cachedToken is the on-disk shape of the gh-session token cache. Same
// schema as the broker's read-token cache, kept duplicated here so the
// shim doesn't depend on internal/broker.
type cachedToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ensureFreshToken returns a token suitable for use now. If the cached
// token is missing or within refreshSkew of expiry, mint a new one,
// atomically replace the cache file, and best-effort revoke the prior
// token to tighten its effective TTL.
func ensureFreshToken(tokenPath string, logger *log.Logger) (string, error) {
	cur, ok := loadCachedToken(tokenPath)
	if ok && time.Until(cur.ExpiresAt) > refreshSkew {
		logger.Printf("cache hit: expires_at=%s ttl=%s",
			cur.ExpiresAt.Format(time.RFC3339),
			time.Until(cur.ExpiresAt).Round(time.Second))
		return cur.Token, nil
	}

	appCfg, err := config.LoadAppConfig()
	if err != nil {
		return "", err
	}
	client, err := githubapp.NewClient(appCfg)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), mintTimeout)
	defer cancel()
	token, expiresAt, err := client.MintGhSessionToken(ctx)
	if err != nil {
		return "", fmt.Errorf("mint gh session token: %w", err)
	}
	logger.Printf("mint succeeded: expires_at=%s ttl=%s token_len=%d",
		expiresAt.Format(time.RFC3339),
		time.Until(expiresAt).Round(time.Second),
		len(token))

	if err := writeCachedToken(tokenPath, cachedToken{Token: token, ExpiresAt: expiresAt}); err != nil {
		logger.Printf("cache write failed (continuing): %v", err)
	}

	// Best-effort revoke of the old token. Done after we have the new
	// token in hand so a revoke failure can't leave us with neither.
	if ok && cur.Token != "" && cur.Token != token {
		revokeCtx, revokeCancel := context.WithTimeout(context.Background(), mintTimeout)
		defer revokeCancel()
		if err := client.RevokeToken(revokeCtx, cur.Token); err != nil {
			logger.Printf("revoke of previous token failed (best-effort): %v", err)
		} else {
			logger.Printf("revoked previous gh-session token")
		}
	}

	return token, nil
}

func loadCachedToken(path string) (cachedToken, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return cachedToken{}, false
	}
	var c cachedToken
	if err := json.Unmarshal(body, &c); err != nil {
		return cachedToken{}, false
	}
	return c, true
}

func writeCachedToken(path string, c cachedToken) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "gh-token.json.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// findRealGh returns the system `gh` executable, deliberately excluding
// this shim's directory. Same approach as cmd/rein-git's findRealGit.
// REIN_REAL_GH env overrides for tests/distributions without gh on PATH.
func findRealGh(shimPath string) (string, error) {
	if override := os.Getenv("REIN_REAL_GH"); override != "" {
		if _, err := os.Stat(override); err == nil {
			return override, nil
		}
		return "", fmt.Errorf("REIN_REAL_GH=%q does not exist", override)
	}

	shimAbs, err := filepath.Abs(shimPath)
	if err != nil {
		shimAbs = shimPath
	}
	if resolved, err := filepath.EvalSymlinks(shimAbs); err == nil {
		shimAbs = resolved
	}
	shimDir := filepath.Dir(shimAbs)

	path := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		if abs == shimDir {
			continue
		}
		cand := filepath.Join(dir, "gh")
		if info, err := os.Stat(cand); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", errors.New("no gh binary found on PATH (excluding rein-gh's own directory)")
}

func openLog(stateDir string) (*log.Logger, func(), error) {
	path := filepath.Join(stateDir, "gh-shim.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log: %w", err)
	}
	return log.New(f, fmt.Sprintf("[pid %d] ", os.Getpid()), log.LstdFlags|log.LUTC),
		func() { _ = f.Close() },
		nil
}

