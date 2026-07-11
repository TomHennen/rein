package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/appsetup"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/session"
)

// TestCredentialHelper_TMG8_OnMissingInstallID is the load-bearing TM-G8
// test. It drives the ACTUAL broker (not a reimplementation) through the
// extracted helper core with an InstallationID==0 config — the state-path-
// uncached case. NewClient rejects id==0 inside the lazy MintRead closure,
// so the broker must fall through to the placeholder credential and the
// call must return nil. A regression here means git falls back to
// `gh auth setup-git` (TM-G8 violation).
func TestCredentialHelper_TMG8_OnMissingInstallID(t *testing.T) {
	stateDir := t.TempDir()
	logger := log.New(io.Discard, "", 0)
	sess := session.Session{ID: "s", Role: "implement", Repos: []string{"owner/name"}}

	appCfg := githubapp.Config{
		ClientID:       "Iv23li-test",
		InstallationID: 0, // uncached -> NewClient will reject inside the closure
		RepoNames:      []string{"name"},
	}
	// A FileKeystore on an empty dir; never actually reached because the
	// id==0 check in NewClient fails first, but it satisfies the signature.
	ks := keystore.NewFileKeystore(t.TempDir())

	in := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var out, diag strings.Builder

	err := runCredentialHelperWithConfig("get", in, &out, &diag, appCfg, ks, sess, stateDir, logger)
	if err != nil {
		t.Fatalf("helper must never error on github.com get (TM-G8): %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "password=rein-placeholder-mint-failed") {
		t.Errorf("expected TM-G8 placeholder credential, got:\n%s", got)
	}
	// stdout must carry ONLY the credential protocol — the diagnostic goes
	// to the separate diag (stderr) sink, never stdout.
	if strings.Contains(got, "rein doctor") {
		t.Errorf("diagnostic leaked onto stdout (corrupts credential protocol):\n%s", got)
	}
	// The agent-facing diagnostic must explain the failure and point at
	// `rein doctor` so a cooperative agent does the right thing.
	if d := diag.String(); !strings.Contains(d, "rein doctor") {
		t.Errorf("expected actionable `rein doctor` diagnostic on diag/stderr, got:\n%s", d)
	}
}

// setupHelperTestEnv redirects HOME/XDG into a temp dir and installs a valid
// env-path App config so config.ResolveApp succeeds without touching disk or
// network. It also pins the session-relevant env vars to a known-clean state
// (empty == unset for all of them). Returns the temp root.
func setupHelperTestEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, ".state"))
	t.Setenv("REIN_APP_CLIENT_ID", "Iv23li-test")
	t.Setenv("REIN_APP_PRIVATE_KEY_PATH", filepath.Join(tmp, "app.pem"))
	t.Setenv("REIN_APP_INSTALLATION_ID", "12345")
	t.Setenv("REIN_TEST_REPO_A", "owner/name")
	t.Setenv("REIN_SESSION_FILE", "")
	t.Setenv("REIN_GIT_OP", "read")
	t.Setenv("REIN_RUN_ID", "")
	return tmp
}

// assertPlaceholderServed asserts the TM-G8 contract for a github.com get
// whose pre-broker setup failed: nil error (exit 0), a non-empty credential
// block on stdout carrying the mint-failed placeholder, no diagnostic leak
// onto stdout, and a stderr diagnostic containing each of wantDiag.
func assertPlaceholderServed(t *testing.T, err error, out, diag string, wantDiag ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("helper must never error on github.com get (TM-G8 / hard-constraint #2): %v", err)
	}
	if !strings.Contains(out, "username=x-access-token") ||
		!strings.Contains(out, "password=rein-placeholder-mint-failed") {
		t.Errorf("expected non-empty TM-G8 placeholder credential block on stdout, got:\n%q", out)
	}
	if strings.Contains(out, "rein doctor") || strings.Contains(out, "rein:") {
		t.Errorf("diagnostic leaked onto stdout (corrupts credential protocol):\n%s", out)
	}
	for _, want := range wantDiag {
		if !strings.Contains(diag, want) {
			t.Errorf("stderr diagnostic missing %q, got:\n%s", want, diag)
		}
	}
}

// driveHelperGet runs the env-driven helper core with a github.com get.
func driveHelperGet(t *testing.T) (err error, out, diag string) {
	t.Helper()
	in := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var outB, diagB strings.Builder
	err = runCredentialHelperEnv("get", in, &outB, &diagB)
	return err, outB.String(), diagB.String()
}

// TestCredentialHelper_TMG8_OnMalformedSessionFile: a corrupted/malformed
// dev-session.yaml at the DEFAULT path must yield the placeholder, not an
// error. An error return would exit 1 with empty stdout, and git treats
// that as "no answer" and falls through to the next credential source —
// potentially the developer's ambient PAT (issue #45).
func TestCredentialHelper_TMG8_OnMalformedSessionFile(t *testing.T) {
	tmp := setupHelperTestEnv(t)
	sessDir := filepath.Join(tmp, ".config", "rein")
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessPath := filepath.Join(sessDir, "dev-session.yaml")
	if err := os.WriteFile(sessPath, []byte("{invalid: [yaml"), 0o600); err != nil {
		t.Fatal(err)
	}

	err, out, diag := driveHelperGet(t)
	// The hint must say exactly what failed (the file path is embedded in
	// the parse error) and the generic diag must point at `rein doctor`.
	assertPlaceholderServed(t, err, out, diag, "dev-session.yaml", "rein doctor")
}

// TestCredentialHelper_TMG8_OnMissingSessionFileEnv: REIN_SESSION_FILE
// naming a nonexistent file is a hard session error (never a silent
// fallback) — but on the github.com get path it must still surface as the
// placeholder + stderr hint, never empty stdout.
func TestCredentialHelper_TMG8_OnMissingSessionFileEnv(t *testing.T) {
	tmp := setupHelperTestEnv(t)
	t.Setenv("REIN_SESSION_FILE", filepath.Join(tmp, "does-not-exist.yaml"))

	err, out, diag := driveHelperGet(t)
	assertPlaceholderServed(t, err, out, diag, "REIN_SESSION_FILE", "does-not-exist.yaml", "rein doctor")
}

// TestCredentialHelper_TMG8_OnNoSessionNoFallback: state-path App config
// (no REIN_APP_* env, so no env fallback repo), no session file at the
// default path — the literal "no session is active" state. Must serve the
// placeholder. Reachable only via state.json config: the env config path
// requires REIN_TEST_REPO_A, which doubles as the fallback repo.
func TestCredentialHelper_TMG8_OnNoSessionNoFallback(t *testing.T) {
	tmp := setupHelperTestEnv(t)
	t.Setenv("REIN_TEST_REPO_A", "") // env config path fails -> state path; no fallback repo
	cfgDir := filepath.Join(tmp, ".config", "rein")
	if err := appsetup.WriteState(cfgDir, appsetup.State{
		Phase:   appsetup.PhaseAuditDone,
		Primary: &appsetup.AppRecord{ClientID: "Iv23li-test", InstallationID: 12345},
	}); err != nil {
		t.Fatal(err)
	}

	err, out, diag := driveHelperGet(t)
	assertPlaceholderServed(t, err, out, diag, "no session file", "rein doctor")
}

// TestCredentialHelper_TMG8_OnStateDirFailure: when the state dir cannot be
// created (here: XDG_STATE_HOME is a FILE), the helper log degrades with a
// warning (fail-open on observability) and the credential path fails closed
// to the placeholder — never empty stdout + exit 1.
func TestCredentialHelper_TMG8_OnStateDirFailure(t *testing.T) {
	tmp := setupHelperTestEnv(t)
	blocker := filepath.Join(tmp, "state-is-a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", blocker)

	err, out, diag := driveHelperGet(t)
	assertPlaceholderServed(t, err, out, diag, "state dir unavailable", "without logging")
}
