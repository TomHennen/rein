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

	"github.com/TomHennen/rein/internal/brokercore"
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

// BareRepoNames returns the "name" halves of the session's "owner/name"
// repos. The App installation pins the owner, so the installation-token mint
// API only accepts bare names; the minted token is scoped to this full set
// (issue #10: token scope == the Contains scope ceiling). An entry without an
// owner is passed through unchanged.
func (s *Session) BareRepoNames() []string {
	names := make([]string, 0, len(s.Repos))
	for _, r := range s.Repos {
		if _, name, ok := strings.Cut(r, "/"); ok {
			names = append(names, name)
		} else {
			names = append(names, r)
		}
	}
	return names
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

// normalizeRepo canonicalizes an "owner/name" for case-insensitive scope
// comparison. It delegates the parsing (strip ".git", trailing slash, take
// the first two segments) to the single canonical helper
// brokercore.RepoFromPath (issue #11 — previously broker.pathToRepo,
// session.normalizeRepo, and the proxy each parsed repo paths their own way),
// then lowercases (GitHub is case-preserving but case-insensitive on path).
// Returns "" if the input isn't owner/name-shaped.
//
// One behavior delta vs. the prior hand-rolled parse: RepoFromPath strips a
// leading "/", so "/owner/name" now normalizes to "owner/name" instead of ""
// — strictly more lenient, and safe (it only ever accepts a genuine
// owner/name, never a malformed string, as in-scope).
func normalizeRepo(s string) string {
	return strings.ToLower(brokercore.RepoFromPath(s))
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
//  1. REIN_SESSION_FILE env override (test/explicit override path). If set
//     but the file is MISSING, that's a hard error — not a fallback. The
//     operator named a specific file; silently using a different scope is a
//     footgun.
//  2. DefaultFilePath() (~/.config/rein/dev-session.yaml).
//  3. Phase 0 dev fallback: a single-repo session scoped to fallbackRepo
//     (typically the value of REIN_TEST_REPO_A). Allows the env-only
//     setup that's been working since CP1-CP3.7 to keep working without
//     forcing every developer to author a session file. Reached ONLY when
//     REIN_SESSION_FILE is unset and the default file is absent.
//
// Returns the session and a short tag describing the source for log
// clarity. A malformed/invalid file is a hard error — better to surface
// than silently fall back.
func LoadOrFallback(fallbackRepo string) (Session, string, error) {
	explicit := os.Getenv("REIN_SESSION_FILE")
	path := explicit
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
	// An explicitly-requested REIN_SESSION_FILE that doesn't exist is a hard
	// error, NOT a silent env-fallback: the operator named a specific file,
	// and quietly running with a different (env-derived) scope is a footgun
	// — it looks like the chosen session is active when it isn't.
	if explicit != "" {
		return Session{}, "", fmt.Errorf("REIN_SESSION_FILE=%s does not exist", explicit)
	}
	// Only the DEFAULT file being absent falls through to the Phase 0
	// env-fallback (the env-only setup that's worked since CP1-CP3.7).
	if normalizeRepo(fallbackRepo) == "" {
		return Session{}, "", fmt.Errorf("no session file at %s and no usable fallback repo", path)
	}
	return Session{
		ID:    "sess_dev_envfallback",
		Role:  "implement",
		Repos: []string{fallbackRepo},
	}, "env-fallback", nil
}
