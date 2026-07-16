// Package config loads rein's Phase 0 configuration from REIN_* environment
// variables. Shared by cmd/rein and cmd/rein-gh so the two binaries see
// the same App and repo configuration.
//
// Phase 0 is single-App, single-repo. CP4+ replaces this with per-session
// config loaded from disk; the env-var path will remain as the developer
// affordance.
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/TomHennen/rein/internal/appsetup"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
)

// AppSource identifies which backend ResolveApp loaded the App config from.
// Callers that must distinguish the env path (install-id already present,
// no state.json to own) from the state path (install-id may be uncached
// and rein run's eager step owns fetching it) branch on this.
type AppSource int

const (
	// SourceNone is the zero value, returned alongside an error.
	SourceNone AppSource = iota
	// SourceEnv means the config came from REIN_APP_* env vars (Phase 0
	// dev path). InstallationID is always non-zero here.
	SourceEnv
	// SourceState means the config came from state.json + keystore
	// (manifest flow). InstallationID may be 0 (uncached) — that is NOT
	// an error; rein run's eager step fetches and caches it.
	SourceState
)

// AppKeystoreRole is the entry name passed to keystore.Get for the env-var
// PEM. Kept as a constant so mint-site callers and any future audit-App
// callers can refer to the same string without typos.
const AppKeystoreRole = "primary"

// LoadAppConfig reads REIN_APP_CLIENT_ID, REIN_APP_PRIVATE_KEY_PATH,
// REIN_APP_INSTALLATION_ID, and REIN_TEST_REPO_A and returns a validated
// githubapp.Config alongside a Keystore that exposes the env-var PEM
// under AppKeystoreRole. Returns a descriptive error if any of the
// required vars are missing or malformed.
//
// The PEM file is NOT stat'd here — stat-and-read happens on first
// keystore.Get inside a mint. Callers that need an early "file present
// and readable" check should stat REIN_APP_PRIVATE_KEY_PATH themselves
// before threading the keystore into NewClient.
func LoadAppConfig() (githubapp.Config, keystore.Keystore, error) {
	required := []string{
		"REIN_APP_CLIENT_ID",
		"REIN_APP_PRIVATE_KEY_PATH",
		"REIN_APP_INSTALLATION_ID",
		"REIN_TEST_REPO_A",
	}
	for _, k := range required {
		if os.Getenv(k) == "" {
			return githubapp.Config{}, nil, fmt.Errorf("missing env var %s (did you source ./dev-env?)", k)
		}
	}
	installationID, err := strconv.ParseInt(os.Getenv("REIN_APP_INSTALLATION_ID"), 10, 64)
	if err != nil {
		return githubapp.Config{}, nil, fmt.Errorf("REIN_APP_INSTALLATION_ID not an int64: %w", err)
	}
	// Reject 0 (and negatives) explicitly. githubapp uses 0 as the "absent"
	// sentinel — NewClient rejects it, and ResolveApp's SourceState path
	// returns 0 to mean "uncached, go fetch it". An env var literally set to
	// 0 is a misconfiguration, not an id; letting it through would make
	// SourceEnv's "InstallationID is always non-zero" contract a lie and
	// silently collide with the sentinel. Fail closed at the source.
	if installationID <= 0 {
		return githubapp.Config{}, nil, fmt.Errorf("REIN_APP_INSTALLATION_ID must be a positive installation id, got %d", installationID)
	}
	slug := os.Getenv("REIN_TEST_REPO_A")
	_, repoName, ok := strings.Cut(slug, "/")
	if !ok || repoName == "" {
		return githubapp.Config{}, nil, fmt.Errorf("REIN_TEST_REPO_A %q is not owner/name", slug)
	}
	cfg := githubapp.Config{
		ClientID:       os.Getenv("REIN_APP_CLIENT_ID"),
		InstallationID: installationID,
		RepoNames:      []string{repoName},
	}
	ks := keystore.NewSingleFileKeystore(AppKeystoreRole, os.Getenv("REIN_APP_PRIVATE_KEY_PATH"))
	return cfg, ks, nil
}

// ResolveApp resolves App config from env-or-state, NEVER touching the
// network. It is the read-only resolver shared by the credential helper,
// rein-gh, ghAuth, doctor, and rein run's pre-launch read.
//
// Precedence:
//  1. REIN_APP_* env vars present and valid -> use them (env wins;
//     unchanged Phase 0 behavior). Source is SourceEnv; InstallationID is
//     non-zero. Reusing LoadAppConfig as the validity oracle keeps this in
//     lockstep with everywhere else that asks "is the env App config valid?"
//  2. else state.json with a manifest phase (primary_done/audit_done) and a
//     Primary record -> client_id from state.json, PEM from keystore[primary],
//     installation_id from state.json IF cached. Source is SourceState;
//     InstallationID may be 0 (uncached) with a NIL error — callers that need
//     a non-zero id construct githubapp.Client LAZILY so a zero id degrades
//     to a mint error (TM-G8 placeholder in the helper), never an early
//     return. RepoNames is left empty; every caller overrides it from the
//     session before constructing the client.
//  3. else -> fail closed.
//
// The InstallationID==0 sentinel for "uncached" reuses githubapp's existing
// "0 means absent" convention (NewClient rejects 0), so no new bool/error
// shape is introduced.
func ResolveApp() (githubapp.Config, keystore.Keystore, AppSource, error) {
	// 1. Env path. LoadAppConfig is the single oracle for "env present and
	//    valid" (requires all four REIN_APP_* vars), so the env-vs-state
	//    decision here matches every other env-validity check in the tree.
	if cfg, ks, err := LoadAppConfig(); err == nil {
		return cfg, ks, SourceEnv, nil
	}

	// Falling through to the state path. (A partial env — some but not all
	// REIN_APP_* identity vars set — is surfaced once at `rein run` launch
	// via WarnPartialAppEnv, NOT here: ResolveApp is on the per-git-op
	// credential-helper hot path and warning here spams stderr on every
	// invocation.)

	// 2. State path.
	configDir, err := ConfigDir()
	if err != nil {
		return githubapp.Config{}, nil, SourceNone, err
	}
	s, serr := appsetup.ReadState(configDir)
	switch {
	case serr == nil:
		// state.json present and parsed; use it if it's a manifest-flow setup.
		if appsetup.IsManifestPhase(s) && s.Primary != nil {
			cfg := githubapp.Config{
				ClientID:       s.Primary.ClientID,
				InstallationID: s.Primary.InstallationID, // may be 0 (uncached)
				// RepoNames intentionally empty; callers set them from the session.
			}
			ks := keystore.NewFileKeystore(configDir)
			return cfg, ks, SourceState, nil
		}
	case errors.Is(serr, fs.ErrNotExist):
		// state.json absent — a fresh install. Fall through to the generic
		// "set REIN_APP_* or run rein init" message below.
	default:
		// state.json present but unreadable/corrupt (parse error). Fail
		// closed WITH the real reason rather than the misleading "run
		// `rein init`" — init would not fix a corrupt file.
		return githubapp.Config{}, nil, SourceNone,
			fmt.Errorf("state.json unreadable: %w", serr)
	}

	// 3. Fail closed.
	return githubapp.Config{}, nil, SourceNone,
		fmt.Errorf("no App config: set REIN_APP_* (source ./dev-env) or run `rein init`")
}

// WarnPartialAppEnv writes a one-line note to w when SOME but not all of the
// REIN_APP_* App-IDENTITY vars are set — a state that means the operator
// likely intended the env App path but a missing var (or typo) made
// LoadAppConfig treat env as absent, so the resolver fell back to state.json.
// Silent on none-set (clean turnkey/state path) and all-set (env path).
//
// Only the three identity vars count; REIN_TEST_REPO_A is the test-repo
// selector, not App identity, and the turnkey path supplies the repo via the
// session file — counting it here would mis-fire for a normal turnkey user
// who happens to have REIN_TEST_REPO_A exported.
//
// Call this ONCE from a user-facing entry point (e.g. `rein run` launch),
// never from the credential-helper hot path. Write to stderr only — stdout
// is the credential protocol channel.
func WarnPartialAppEnv(w io.Writer) {
	vars := []string{
		"REIN_APP_CLIENT_ID",
		"REIN_APP_PRIVATE_KEY_PATH",
		"REIN_APP_INSTALLATION_ID",
	}
	set := 0
	for _, v := range vars {
		if os.Getenv(v) != "" {
			set++
		}
	}
	if set > 0 && set < len(vars) {
		fmt.Fprintln(w, "note: partial REIN_APP_* env (some App identity vars set, some missing); ignoring env and using state.json — set all three or none")
	}
}

// StateDir is $XDG_STATE_HOME/rein (defaulting to ~/.local/state/rein).
// Created with mode 0700 on first use.
func StateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home dir: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "rein")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	return dir, nil
}

// ConfigDir is $XDG_CONFIG_HOME/rein (defaulting to ~/.config/rein).
// Does NOT create the directory — config files are user-edited.
func ConfigDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home dir: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "rein"), nil
}

// SandboxClaudeHomeDir is the rein-owned, PERSISTENT CLAUDE_CONFIG_DIR overlay
// for sandboxed claude runs: $XDG_CONFIG_HOME/rein-sandbox-home/.claude
// (default ~/.config/rein-sandbox-home/.claude). Issue #94.
//
// Deliberately a SIBLING of ConfigDir, NOT nested under it: ConfigDir holds the
// proxy CA key and is denied WHOLESALE in-sandbox (credentialDenyReadPaths), so
// an overlay under it would (a) collide with that authoritative deny — srt.Build
// fails closed on a widening path at/under a deny — and (b) need to be
// agent-READABLE, contradicting the deny. Persistent (not tmpfs) so claude
// sessions resume across runs; a single SHARED dir (rein sessions span multiple
// repos, so there is no clean repo key — mirrors host claude's one ~/.claude).
// Does NOT create the directory (the caller creates it 0700 and seeds it).
func SandboxClaudeHomeDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home dir: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "rein-sandbox-home", ".claude"), nil
}
