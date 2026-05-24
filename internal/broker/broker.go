// Package broker implements the git credential-helper protocol on top of
// a github-app-backed token minter.
//
// The defining invariant is TM-G8 (design §5.3): for any github.com get
// request, the helper MUST exit 0 with a non-empty credential block — never
// empty, never error. An empty/error return triggers downstream agents
// (validated against Claude Code in §12.1) to run `gh auth setup-git`,
// silently rewriting ~/.gitconfig and displacing the broker. The placeholder
// path in handleGet is what enforces this when the real mint fails.
//
// Non-github.com hosts get an empty credential block on purpose — that is
// the credential-helper protocol's "I don't handle this host" signal, and
// the TM-G8 self-remediation concern only applies to the github.com path
// the agent is being prevented from rewriting.
package broker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"
	"time"
)

// MintFunc mints a fresh read-only installation token. Returned as a
// function (rather than an interface) so tests can stub trivially.
type MintFunc func(ctx context.Context) (token string, expiresAt time.Time, err error)

// Config controls credential-helper behavior.
type Config struct {
	// Mint produces an installation token for the configured repo. Required.
	Mint MintFunc

	// MintTimeout caps each mint attempt. On timeout we fall back to the
	// placeholder (TM-G8). 5s is comfortable for the GitHub installation
	// token API in normal conditions; git users will feel a longer wait.
	MintTimeout time.Duration

	// Logger is used for forensic logging. Required. The helper must never
	// log raw token values — only metadata (expiry, length, scope).
	Logger *log.Logger
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
	if cfg.Mint == nil {
		return fmt.Errorf("broker: Mint is required")
	}

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
		// Stateless helper; nothing to persist or forget.
		return nil
	case "get":
		return handleGet(attrs, stdout, cfg)
	default:
		// Unknown verb — treat as no-op, do not error.
		cfg.Logger.Printf("unknown action %q; no-op", action)
		return nil
	}
}

// handleGet is the TM-G8-bearing path. Split out for direct testing.
func handleGet(attrs map[string]string, stdout io.Writer, cfg Config) error {
	host := attrs["host"]
	protocol := attrs["protocol"]

	// Only github.com over HTTPS is in scope. SSH (and any other protocol)
	// uses key-based auth and would just fail with a Bearer token; declining
	// is correct. Non-github.com hosts likewise fall through.
	if host != "github.com" || protocol != "https" {
		cfg.Logger.Printf("not handled: protocol=%q host=%q; returning empty", protocol, host)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.MintTimeout)
	defer cancel()

	token, expiresAt, err := cfg.Mint(ctx)
	if err != nil {
		// TM-G8: never empty for github.com get. Return a placeholder.
		// For CP2 a non-empty sentinel satisfies the helper-level invariant
		// ("never empty, never error"); git will fail with 401 against it.
		// Phase 1 should harden this into a real-but-narrow read-only token
		// so a sufficiently determined agent doesn't react to the 401 by
		// running `gh auth setup-git` anyway.
		cfg.Logger.Printf("mint failed: %v; returning TM-G8 placeholder credential", err)
		return writeCredential(stdout, "x-access-token", "rein-placeholder-mint-failed")
	}

	cfg.Logger.Printf("mint succeeded: expires_at=%s ttl=%s token_len=%d",
		expiresAt.Format(time.RFC3339),
		time.Until(expiresAt).Round(time.Second),
		len(token))
	return writeCredential(stdout, "x-access-token", token)
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
