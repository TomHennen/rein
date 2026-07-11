package main

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/tokencache"
)

// These tests pin the two-tier read/mint routing and the denial env
// (audit #44 §2). They run rein-gh's real code paths in-process with a
// fake `gh` (a shell script that dumps its credential env to a file) and
// never touch the network: the read tier is served from a pre-seeded
// fresh cache, and the write tier's mint fails fast at PEM parse — before
// any HTTP — via a deliberately-garbage key file.
//
// NOT covered here (needs a production seam or a tty; noted as follow-up
// in the audit issue):
//   - a write-tier mint SUCCEEDING (would need an API-base seam reachable
//     from cmd/rein-gh, beyond the unexported internal/githubapp one)
//   - the interactive approval-denied leg (grant_test.go covers the
//     decision with a stub prompter; the tty ceremony is interactive-suite
//     territory, and the gh-shim leg needs direct-mode harness machinery
//     that tests/interactive does not have yet)

// testLogger returns a logger that records into the test log.
func testLogger(t *testing.T) *log.Logger {
	return log.New(&testLogWriter{t}, "", 0)
}

type testLogWriter struct{ t *testing.T }

func (w *testLogWriter) Write(b []byte) (int, error) {
	w.t.Logf("rein-gh: %s", strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// writeFakeGh writes a fake `gh` that dumps the credential-relevant env
// vars to $REIN_TEST_GH_ENVOUT and exits with the given code.
func writeFakeGh(t *testing.T, exitCode int) (ghPath, envOutPath string) {
	t.Helper()
	dir := t.TempDir()
	envOutPath = filepath.Join(dir, "env-out")
	ghPath = filepath.Join(dir, "gh")
	script := fmt.Sprintf(`#!/bin/sh
{
  printf 'GH_TOKEN=%%s\n' "$GH_TOKEN"
  printf 'GITHUB_TOKEN=%%s\n' "$GITHUB_TOKEN"
  printf 'GH_ENTERPRISE_TOKEN=%%s\n' "$GH_ENTERPRISE_TOKEN"
  printf 'GITHUB_ENTERPRISE_TOKEN=%%s\n' "$GITHUB_ENTERPRISE_TOKEN"
  printf 'REIN_GH_SHIM_ACTIVE=%%s\n' "$REIN_GH_SHIM_ACTIVE"
} > "$REIN_TEST_GH_ENVOUT"
exit %d
`, exitCode)
	if err := os.WriteFile(ghPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("REIN_TEST_GH_ENVOUT", envOutPath)
	return ghPath, envOutPath
}

// readEnvOut parses the fake gh's env dump.
func readEnvOut(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("fake gh did not run (no env dump): %v", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			out[k] = v
		}
	}
	return out
}

// genTestPEM mirrors internal/githubapp's test helper: a valid RSA PKCS#1
// PEM so config loading and (if reached) key parsing succeed.
func genTestPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// setupAppEnv points the REIN_APP_* env at a repo-only session on
// owner/alpha, with the App PEM at pemPath. Returns the loaded session.
// (The session carries no `issue:`: since issue #35 that field is retired
// — the issue is agent-declared at runtime and lives in the run's
// approval record, not the session file.)
func setupAppEnv(t *testing.T, pemPath string) session.Session {
	t.Helper()
	t.Setenv("REIN_APP_CLIENT_ID", "Iv23li-test")
	t.Setenv("REIN_APP_INSTALLATION_ID", "42")
	t.Setenv("REIN_APP_PRIVATE_KEY_PATH", pemPath)
	t.Setenv("REIN_TEST_REPO_A", "owner/alpha")

	sessPath := filepath.Join(t.TempDir(), "dev-session.yaml")
	yaml := `id: sess_routing_test
role: implement
repos:
  - owner/alpha
`
	if err := os.WriteFile(sessPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	t.Setenv("REIN_SESSION_FILE", sessPath)

	sess, err := session.LoadFromFile(sessPath)
	if err != nil {
		t.Fatalf("load session back: %v", err)
	}

	// Keep the ambient terminal out of the picture: no tmux popup, and no
	// re-entry short-circuit or stray tokens inherited from the dev shell.
	t.Setenv("TMUX", "")
	t.Setenv("REIN_APPROVAL", "")
	t.Setenv(reentryEnv, "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("REIN_RUN_PID", "")
	return sess
}

// TestExecGhWithoutToken_DenialEnv pins the TM-G8-mirroring denial env
// (audit #44 §2): the denial path must hand gh a DELIBERATELY-INVALID
// GH_TOKEN (so gh fails loudly instead of silently falling back to the
// user's hosts.yml PAT), empty GITHUB_TOKEN, set the re-entry guard, and
// pass gh's exit code through.
func TestExecGhWithoutToken_DenialEnv(t *testing.T) {
	ghPath, envOut := writeFakeGh(t, 7)
	// Poison the inherited env with "user credentials": the denial path
	// must OVERRIDE both, not let gh see them.
	t.Setenv("GH_TOKEN", "users-real-pat")
	t.Setenv("GITHUB_TOKEN", "users-real-pat")

	rc := execGhWithoutToken(ghPath, []string{"issue", "comment", "1"})

	if rc != 7 {
		t.Errorf("exit code = %d, want 7 (gh's own exit code passed through)", rc)
	}
	env := readEnvOut(t, envOut)
	if got := env["GH_TOKEN"]; got != "rein-placeholder-denied" {
		t.Errorf("GH_TOKEN = %q, want %q", got, "rein-placeholder-denied")
	}
	if got := env["GITHUB_TOKEN"]; got != "" {
		t.Errorf("GITHUB_TOKEN = %q, want empty (must not leak the inherited value)", got)
	}
	assertNoInheritedPAT(t, env)
	if got := env["REIN_GH_SHIM_ACTIVE"]; got != "1" {
		t.Errorf("REIN_GH_SHIM_ACTIVE = %q, want %q (re-entry guard on the denial path)", got, "1")
	}
}

// TestReadTierToken_ServesFromReadCache pins the read tier's routing: a
// fresh cached token at ghsession.ReadCachePath is served as-is — no mint,
// no write-tier involvement. (A mint here would fail loudly: the fake app
// config points at an unreachable installation, and no network is
// available to the test by construction of the fresh cache entry.)
func TestReadTierToken_ServesFromReadCache(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(pemPath, genTestPEM(t), 0o600); err != nil {
		t.Fatalf("write pem: %v", err)
	}
	setupAppEnv(t, pemPath)

	stateDir := t.TempDir()
	entry := tokencache.Entry{Token: "cached-read-token", ExpiresAt: time.Now().Add(time.Hour)}
	if err := tokencache.Write(ghsession.ReadCachePath(stateDir), entry); err != nil {
		t.Fatalf("seed read cache: %v", err)
	}

	got := readTierToken(stateDir, testLogger(t))
	if got != "cached-read-token" {
		t.Errorf("readTierToken = %q, want the cached read-tier token", got)
	}
}

// TestReadTierToken_NoConfigReturnsEmpty pins the documented degradation:
// with no resolvable App config the read tier returns "" (gh then surfaces
// its own auth error) rather than erroring or minting with something else.
func TestReadTierToken_NoConfigReturnsEmpty(t *testing.T) {
	for _, k := range []string{"REIN_APP_CLIENT_ID", "REIN_APP_INSTALLATION_ID", "REIN_APP_PRIVATE_KEY_PATH", "REIN_TEST_REPO_A"} {
		t.Setenv(k, "")
	}
	// Point the state path at an empty dir so ResolveApp's state fallback
	// finds nothing either.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if got := readTierToken(t.TempDir(), testLogger(t)); got != "" {
		t.Errorf("readTierToken = %q with no App config, want empty", got)
	}
}

// TestRunWrite_MintFailureNeverFallsBackToReadCacheOrPAT pins the write
// tier's routing invariants with the write gate satisfied by a real
// on-disk per-run approval record carrying a CONFIRMED ISSUE (issue #35:
// the gate is a read of the run's confirmed-issue set — the same record
// `rein declare <n>` + the human's confirmation writes), so no tty is
// involved:
//
//   - the write path attempts a FRESH mint (which fails fast at PEM parse
//     here — no network) and on failure execs gh with NO token: it must
//     never serve a write from the cached READ-tier token sitting right
//     there in stateDir, and never leak an inherited user credential;
//   - gh still runs and its exit code passes through (documented
//     degradation: gh surfaces its own auth error).
//
// The #35 TM-G6 transfer re-check runs before the mint and is a no-op
// here: the confirmed issue has no canonical URL, so CheckCanonical fails
// LOCALLY (no network) with a "cannot verify" error, which is keep-and-log
// — the confirmation survives and control reaches the mint failure this
// test exists to pin.
func TestRunWrite_MintFailureNeverFallsBackToReadCacheOrPAT(t *testing.T) {
	// Garbage PEM: the write-tier mint dies at key parse, before any HTTP.
	pemPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(pemPath, []byte("not a private key"), 0o600); err != nil {
		t.Fatalf("write pem: %v", err)
	}
	sess := setupAppEnv(t, pemPath)

	stateDir := t.TempDir()

	// A fresh READ-tier token is cached — the write path must ignore it.
	entry := tokencache.Entry{Token: "cached-read-token", ExpiresAt: time.Now().Add(time.Hour)}
	if err := tokencache.Write(ghsession.ReadCachePath(stateDir), entry); err != nil {
		t.Fatalf("seed read cache: %v", err)
	}

	// Satisfy the write gate the way a live run does: a valid per-run
	// approval record keyed to this session's signature AND carrying a
	// confirmed issue (an empty Issues set is a DENY under #35 — the shim
	// would serve the placeholder and never reach the mint path below).
	const runID = "run-routing-test"
	t.Setenv("REIN_RUN_ID", runID)
	rec := approvals.Record{
		Signature:  approvals.SignatureOf(sess),
		SessionID:  sess.ID,
		Issues:     []approvals.ConfirmedIssue{{Number: 123, Repo: "owner/alpha", Title: "t", State: "open", ConfirmedAt: time.Now()}},
		ApprovedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if err := approvals.WriteApproval(stateDir, runID, rec); err != nil {
		t.Fatalf("seed approval record: %v", err)
	}

	// Poison the inherited environment the way a real developer shell does
	// (issue #57). setupAppEnv blanks these, which is exactly why the
	// original version of this test could not see the leak: with no ambient
	// PAT to inherit, "GH_TOKEN is empty at gh" looked like a pass. Set them
	// AFTER setupAppEnv so gh would receive them if the shim passed the
	// environment through untouched.
	t.Setenv("GH_TOKEN", "ghp_users_real_full_scope_pat")
	t.Setenv("GITHUB_TOKEN", "ghp_users_real_full_scope_pat")
	t.Setenv("GH_ENTERPRISE_TOKEN", "ghp_users_real_full_scope_pat")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "ghp_users_real_full_scope_pat")

	ghPath, envOut := writeFakeGh(t, 3)
	rc := runWrite(ghPath, []string{"issue", "comment", "1", "-b", "x"}, stateDir, testLogger(t))

	if rc != 3 {
		t.Errorf("exit code = %d, want 3 (gh's exit code passed through)", rc)
	}
	env := readEnvOut(t, envOut)
	// Fail-closed: gh gets the deliberately-invalid placeholder, NOT the
	// cached read token, NOT the inherited PAT, and NOT an empty GH_TOKEN
	// (empty would let gh fall back to the user's hosts.yml login).
	if got := env["GH_TOKEN"]; got != placeholderToken {
		t.Errorf("GH_TOKEN = %q after write-tier mint failure, want %q — the write path must not serve the cached read token, an inherited PAT, or an empty value that lets gh fall back to hosts.yml",
			got, placeholderToken)
	}
	assertNoInheritedPAT(t, env)
	if got := env["REIN_GH_SHIM_ACTIVE"]; got != "1" {
		t.Errorf("REIN_GH_SHIM_ACTIVE = %q, want \"1\" (re-entry guard set on the denial path)", got)
	}

	// The read cache must be untouched by the write path (writes are never
	// cached, and a failed write mint must not evict the read tier).
	after, err := tokencache.Read(ghsession.ReadCachePath(stateDir))
	if err != nil || after.Token != "cached-read-token" {
		t.Errorf("read cache after write attempt = (%+v, %v), want the seeded entry untouched", after, err)
	}
}

// assertNoInheritedPAT fails if any ambient GitHub credential env var
// survived into gh's environment. This is the core issue-#57 assertion: the
// shim must strip every var in tokenEnvVars, not just overwrite GH_TOKEN.
func assertNoInheritedPAT(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range tokenEnvVars {
		got := env[name]
		if got == placeholderToken {
			continue // our own deliberately-invalid value, not an inherited one
		}
		if got != "" {
			t.Errorf("%s = %q reached gh — an ambient credential from the developer's shell leaked past the scope ceiling (#57)", name, got)
		}
	}
}

// TestBaseEnv_StripsEveryAmbientToken pins the scrub itself: baseEnv must
// remove every var in tokenEnvVars (the same list `rein run` scrubs) and
// leave everything else alone.
func TestBaseEnv_StripsEveryAmbientToken(t *testing.T) {
	for _, name := range tokenEnvVars {
		t.Setenv(name, "ghp_users_real_full_scope_pat")
	}
	t.Setenv("PATH_LIKE_UNRELATED_VAR", "keep-me")

	got := baseEnv()

	for _, kv := range got {
		name, _, _ := strings.Cut(kv, "=")
		for _, banned := range tokenEnvVars {
			if name == banned {
				t.Errorf("baseEnv() still carries %q", kv)
			}
		}
	}
	if !slices.Contains(got, "PATH_LIKE_UNRELATED_VAR=keep-me") {
		t.Errorf("baseEnv() dropped an unrelated var; want it preserved")
	}
}

// TestDenyEnv_FailsClosedOverInheritedPAT pins the environment the READ-tier
// degraded leg now hands gh (issue #57, read-tier half). runReadAndExec
// syscall.Execs and so cannot be driven in-process; denyEnv is exactly the
// environment it passes, and the end-to-end read leg is covered by the
// manual demo.
func TestDenyEnv_FailsClosedOverInheritedPAT(t *testing.T) {
	for _, name := range tokenEnvVars {
		t.Setenv(name, "ghp_users_real_full_scope_pat")
	}

	env := map[string]string{}
	for _, kv := range denyEnv() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}

	if got := env["GH_TOKEN"]; got != placeholderToken {
		t.Errorf("GH_TOKEN = %q, want the deliberately-invalid placeholder %q", got, placeholderToken)
	}
	assertNoInheritedPAT(t, env)
	if got := env["REIN_GH_SHIM_ACTIVE"]; got != "1" {
		t.Errorf("REIN_GH_SHIM_ACTIVE = %q, want \"1\"", got)
	}
}
