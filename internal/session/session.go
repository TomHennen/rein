// Package session represents a rein session — the unit of agent
// activity bound to a scope ceiling (a list of repositories) and,
// eventually (CP5+), to one or more issues.
//
// In Phase 0 CP4 the session is a static YAML file loaded at startup
// of each helper invocation. CP4 supports exactly the fields needed
// for scope-ceiling enforcement:
//
//   - ID (forensic identifier, surfaced in audit logs)
//   - Role (informational for CP4; gates permissions starting CP5+)
//   - Repos (the scope ceiling — only these repos can be touched)
//
// Future checkpoints add: timing fields (created_at, hard_ttl, idle_ttl)
// in CP5+; bound issues in CP5; SPIFFE-shaped identity strings for
// SLSA policy in CP6+. The struct is intentionally tiny so it can grow
// without churning the package's consumers.
package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Session is the on-disk shape of a rein session config.
//
// YAML field tags use snake_case to be friendly for hand-editing
// (`dev-session.yaml`); Go field names use idiomatic Go.
type Session struct {
	// ID is a forensic identifier surfaced in audit logs. Free-form
	// string; for Phase 0 dev sessions, "sess_dev_xxx" is fine.
	ID string `yaml:"id"`

	// Role names the design's role (§4.2.2): scan, triage, implement,
	// review, release. CP4 doesn't gate on it (the permission set is
	// still hardcoded in the githubapp mint functions); CP5+ wires
	// the role catalog into permission selection.
	Role string `yaml:"role"`

	// Repos is the scope ceiling. Each entry is "owner/name" in
	// GitHub's case (the broker normalizes for comparison). At least
	// one entry is required for a meaningful session.
	Repos []string `yaml:"repos"`

	// Created records when the session was minted, for audit. Optional
	// in the file; if missing, treated as "Phase 0 dev session, age
	// unknown". CP5+ may make this required and enforce hard_ttl
	// against it.
	Created time.Time `yaml:"created_at,omitempty"`

	// Issue is the bound GitHub issue number for human confirmation
	// (CP5). When a write operation requires confirmation, the human
	// types this number to approve. Different sessions bound to
	// different issues have different correct answers — that's the
	// non-replayability property per design §2.2.
	//
	// A zero value means "no confirmation required" — sessions
	// authored before CP5 (env-fallback included) keep working without
	// the prompt. The prompt is opt-in via the file's `issue:` field.
	Issue int `yaml:"issue,omitempty"`
}

// Contains reports whether the given owner/name is within this
// session's scope ceiling. The comparison is case-insensitive (GitHub
// is case-preserving but case-insensitive on path), and tolerates a
// trailing ".git" or "/" on the input.
func (s *Session) Contains(repo string) bool {
	want := normalizeRepo(repo)
	if want == "" {
		return false
	}
	for _, r := range s.Repos {
		if normalizeRepo(r) == want {
			return true
		}
	}
	return false
}

// normalizeRepo strips ".git" suffix (case-insensitive), trims trailing
// slashes, takes the first two slash-separated segments (owner/name),
// and lowercases. Returns "" if the input doesn't have an owner/name
// shape.
func normalizeRepo(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "/")
	// Case-insensitive ".git" strip.
	if len(s) >= 4 && strings.EqualFold(s[len(s)-4:], ".git") {
		s = s[:len(s)-4]
	}
	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return strings.ToLower(parts[0] + "/" + parts[1])
}

// LoadFromFile parses a YAML session file at path. Returns an error
// distinguishable as os.ErrNotExist if the file is missing — callers
// typically fall back to a default in that case.
//
// The file is validated: missing/empty fields are an error EXCEPT for
// Created (optional). An invalid Role is logged at the caller but not
// rejected (the role catalog isn't enforced until CP5+).
func LoadFromFile(path string) (Session, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Session{}, err
	}
	var s Session
	if err := yaml.Unmarshal(body, &s); err != nil {
		return Session{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return Session{}, fmt.Errorf("invalid session %s: %w", path, err)
	}
	return s, nil
}

// Validate checks structural invariants. A session is usable iff it
// has an ID and at least one entry in Repos.
func (s *Session) Validate() error {
	if strings.TrimSpace(s.ID) == "" {
		return errors.New("session.id is required")
	}
	if len(s.Repos) == 0 {
		return errors.New("session.repos must have at least one entry")
	}
	for i, r := range s.Repos {
		if normalizeRepo(r) == "" {
			return fmt.Errorf("session.repos[%d] = %q is not owner/name", i, r)
		}
	}
	return nil
}

// DefaultFilePath returns the canonical session file path under
// $XDG_CONFIG_HOME/rein (defaulting to ~/.config/rein). Does NOT
// create the directory; callers use this only to read.
func DefaultFilePath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home dir: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "rein", "dev-session.yaml"), nil
}

// LoadOrFallback returns the active session for the current invocation.
// Lookup order (each step short-circuits on success):
//
//  1. REIN_SESSION_FILE env override (test/explicit override path).
//  2. DefaultFilePath() (~/.config/rein/dev-session.yaml).
//  3. Phase 0 dev fallback: a single-repo session scoped to fallbackRepo
//     (typically the value of REIN_TEST_REPO_A). Allows the env-only
//     setup that's been working since CP1-CP3.7 to keep working without
//     forcing every developer to author a session file.
//
// Returns the session and a short tag describing the source for log
// clarity. A malformed/invalid file at step 2 is a hard error — better
// to surface than silently fall back.
func LoadOrFallback(fallbackRepo string) (Session, string, error) {
	path := os.Getenv("REIN_SESSION_FILE")
	if path == "" {
		p, err := DefaultFilePath()
		if err != nil {
			return Session{}, "", err
		}
		path = p
	}
	s, err := LoadFromFile(path)
	if err == nil {
		return s, "file:" + path, nil
	}
	if !os.IsNotExist(err) {
		return Session{}, "", err
	}
	if normalizeRepo(fallbackRepo) == "" {
		return Session{}, "", fmt.Errorf("no session file at %s and no usable fallback repo", path)
	}
	return Session{
		ID:    "sess_dev_envfallback",
		Role:  "implement",
		Repos: []string{fallbackRepo},
	}, "env-fallback", nil
}
