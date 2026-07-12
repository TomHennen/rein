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
	"io"
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

	// Issue is IGNORED (issue #35): the issue is agent-declared at
	// runtime via `rein declare <n>` and human-confirmed per run — never
	// pre-configured in the session file. The field is still PARSED so a
	// legacy file loads, but nothing gates on it; WarnIgnoredIssue prints
	// a loud launch-time warning when it is set (silently ignoring a
	// security-looking field is not acceptable). It will be removed once
	// dogfood confirms no files still carry it.
	Issue int `yaml:"issue,omitempty"`

	// AllowDomains is the per-session EXTRA egress allowlist (CP4.5): hosts the
	// sandboxed agent may reach IN ADDITION to GitHub and the built-in agent
	// endpoint — e.g. registry.npmjs.org, pypi.org, files.pythonhosted.org. Each
	// is egress-allowed but NEVER injected with a rein credential (a non-GitHub
	// host gets a direct TLS tunnel to itself). Merged as a UNION with the
	// built-in default and REIN_ALLOW_DOMAINS. Entries are bare hosts or strict
	// `*.suffix` wildcards; a wildcard or a large set triggers a loud egress
	// warning at launch because broad egress is a data-exfiltration surface.
	// Optional and empty by default (Sandboxed mode only; ignored in direct mode).
	AllowDomains []string `yaml:"allow_domains,omitempty"`

	// Worktrees maps a session repo ("owner/name") to the ABSOLUTE path of the
	// developer's EXISTING local checkout of it (issue #64). Each mapped
	// checkout is bind-mounted READ-WRITE into the sandbox at launch, so the
	// agent works in the developer's real tree instead of an ephemeral clone —
	// Tom on PR #56: "I had expected it to work in my local copy."
	//
	// Launch-time only, by construction: bwrap binds are fixed when the sandbox
	// starts, so a repo approved MID-RUN can never get its checkout bound this
	// run (it clones into the ephemeral REIN_EPHEMERAL_CLONE_DIR instead, and
	// its checkout lights up on the NEXT run once mapped here).
	//
	// The repo the developer is STANDING IN needs no entry — it is the working
	// tree, already writable, and its repo is autodetected from its git remote
	// (docs/session-scope-ux-mocks.md §3). This map is for the OTHER repos.
	//
	// Validated fail-closed at launch (internal/worktree.Resolve): the key must
	// be one of Repos, the path must exist, be a git checkout, and its `origin`
	// remote must actually be that GitHub repo. Structural checks (key shape,
	// key in Repos, path absolute) also run in Validate, so a typo surfaces on
	// load rather than at bind time. Optional; sandboxed mode only (direct mode
	// already sees the whole filesystem).
	Worktrees map[string]string `yaml:"worktrees,omitempty"`
}

// BareRepoNames returns the "name" halves of the session's "owner/name"
// repos. The App installation pins the owner, so the installation-token mint
// API only accepts bare names; the minted token is scoped to this full set
// (issue #10: token scope == the Contains scope ceiling).
//
// The bare name is derived from the NORMALIZED form (brokercore.RepoFromPath —
// the same parser Validate/Contains use), NOT a raw strings.Cut: a session
// entry like "owner/name.git", "/owner/name", or "owner/name/" Validates fine
// but a raw Cut would yield "name.git" / a leading-slash-mangled owner, which
// GitHub 422s at mint → every credential silently degrades to the mint-failed
// placeholder (proxy 502) with nothing pointing at the cause (issue #10 F2).
// Case is preserved (RepoFromPath does not lowercase); GitHub matches repo
// names case-insensitively at mint. An unparseable entry is skipped (Validate
// already rejects those, so this is defensive).
func (s *Session) BareRepoNames() []string {
	names := make([]string, 0, len(s.Repos))
	for _, r := range s.Repos {
		norm := brokercore.RepoFromPath(r)
		if _, name, ok := strings.Cut(norm, "/"); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// WarnIgnoredIssue prints the loud, launch-time warning when a session
// file still carries the retired `issue:` field (issue #35): the field
// gates NOTHING anymore — the issue is agent-declared (`rein declare <n>`)
// and human-confirmed per run. Callers (the `rein run` banners, both
// modes) invoke this once at launch; per-op processes (credential helper,
// rein-gh) do not, to avoid stderr spam.
func (s *Session) WarnIgnoredIssue(w io.Writer) {
	if s.Issue == 0 {
		return
	}
	fmt.Fprintf(w, "rein: WARNING: session `issue: %d` is IGNORED — the issue is agent-declared now.\n", s.Issue)
	fmt.Fprintln(w, "      Remove `issue:` from the session file. The agent runs `rein declare <n>`;")
	fmt.Fprintln(w, "      you confirm on this terminal; then writes flow for this run.")
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
	// All repos must share ONE owner. The App installation is single-owner and
	// the minted token scopes by bare repo NAME against that installation
	// (BareRepoNames drops the owner), so a mixed-owner session would mint a
	// token whose scope is ambiguous — worse, "alice/y" in the list against
	// alice's installation could silently grant "alice/y" even if the operator
	// meant "bob/y" (issue #10 hardening: keep token scope == the stated
	// ceiling). Fail closed on mixed owners.
	var owner string
	for i, r := range s.Repos {
		n := normalizeRepo(r)
		if n == "" {
			return fmt.Errorf("session.repos[%d] = %q is not owner/name", i, r)
		}
		o, _, _ := strings.Cut(n, "/")
		if owner == "" {
			owner = o
		} else if o != owner {
			return fmt.Errorf("session.repos mixes owners (%q and %q); a session must be scoped to a single owner (the App installation is single-owner)", owner, o)
		}
	}
	// worktrees: structural checks only (issue #64). The filesystem checks —
	// does the path exist, is it a git checkout, is its origin remote really
	// this repo — happen at LAUNCH (internal/worktree.Resolve), where they can
	// fail the run closed; a session file must stay loadable by the credential
	// helper on a machine where the checkout has since moved. What IS enforced
	// here is what can never be right: a key that isn't owner/name, a key
	// outside the scope ceiling (a writable tree for a repo rein will never
	// mint a credential for is incoherent), or a relative path.
	for repo, path := range s.Worktrees {
		if normalizeRepo(repo) == "" {
			return fmt.Errorf("session.worktrees key %q is not owner/name", repo)
		}
		if !s.Contains(repo) {
			return fmt.Errorf("session.worktrees maps %q, which is not in session.repos; add the repo to the scope ceiling first (a writable checkout for an out-of-scope repo could never be pushed)", repo)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("session.worktrees[%s] = %q must be an absolute path", repo, path)
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
