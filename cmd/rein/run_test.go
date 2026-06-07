package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/appsetup"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/session"
)

func TestParseRunArgs(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		wantErr bool
		wantCmd []string
	}{
		{"no args", nil, true, nil},
		{"just dashes", []string{"--"}, true, nil},
		{"no separator", []string{"claude"}, true, nil},
		{"separator + cmd", []string{"--", "claude"}, false, []string{"claude"}},
		{"separator + cmd + args", []string{"--", "bash", "-c", "echo hi"}, false, []string{"bash", "-c", "echo hi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRunArgs(tc.argv)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if len(got) != len(tc.wantCmd) {
					t.Fatalf("cmd = %v, want %v", got, tc.wantCmd)
				}
				for i := range got {
					if got[i] != tc.wantCmd[i] {
						t.Errorf("cmd[%d] = %q, want %q", i, got[i], tc.wantCmd[i])
					}
				}
			}
		})
	}
}

func TestWriteRunGitConfig_IncludesUserConfig(t *testing.T) {
	// Make a fake user gitconfig so the include.path line is emitted.
	home := t.TempDir()
	t.Setenv("HOME", home)
	userCfg := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(userCfg, []byte("[user]\n  name = test\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	path := filepath.Join(t.TempDir(), "out.gitconfig")
	if err := writeRunGitConfig(path, "/path/to/rein"); err != nil {
		t.Fatalf("writeRunGitConfig: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(body)
	checks := []string{
		"[include]",
		"path = " + userCfg,
		"[credential \"https://github.com\"]",
		"helper =",
		"/path/to/rein credential-helper",
		"useHttpPath = true",
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("gitconfig missing %q\n--- contents ---\n%s", c, s)
		}
	}
}

func TestWriteRunGitConfig_NoUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// No ~/.gitconfig file.
	path := filepath.Join(t.TempDir(), "out.gitconfig")
	if err := writeRunGitConfig(path, "/r"); err != nil {
		t.Fatalf("writeRunGitConfig: %v", err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "[include]") {
		t.Errorf("should not include user config when ~/.gitconfig is absent\n%s", string(body))
	}
	if !strings.Contains(string(body), "credential.https") && !strings.Contains(string(body), `[credential "https://github.com"]`) {
		t.Errorf("should still write credential helper config\n%s", string(body))
	}
}

func TestSetEnv(t *testing.T) {
	env := []string{"FOO=1", "BAR=2", "BAZ=3"}
	got := setEnv(env, "BAR", "new")
	wantHas := "BAR=new"
	wantNot := "BAR=2"
	if !contains(got, wantHas) {
		t.Errorf("missing %q in %v", wantHas, got)
	}
	if contains(got, wantNot) {
		t.Errorf("still has %q in %v", wantNot, got)
	}

	got = setEnv(env, "NEW", "appended")
	if !contains(got, "NEW=appended") {
		t.Errorf("missing appended NEW=appended in %v", got)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// clearAppEnv + state-dir setup for the eager-step tests. The state path is
// taken only when all REIN_APP_* are absent.
func eagerStateDir(t *testing.T) string {
	t.Helper()
	for _, k := range []string{
		"REIN_APP_CLIENT_ID",
		"REIN_APP_PRIVATE_KEY_PATH",
		"REIN_APP_INSTALLATION_ID",
		"REIN_TEST_REPO_A",
	} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "rein")
}

func seedPrimaryState(t *testing.T, configDir string, installID int64) {
	t.Helper()
	if err := appsetup.WriteState(configDir, appsetup.State{
		Phase: appsetup.PhasePrimaryDone,
		Primary: &appsetup.AppRecord{
			ClientID:       "Iv23li-test",
			InstallationID: installID,
			Slug:           "rein-test",
			HTMLURL:        "https://github.com/apps/rein-test",
		},
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

func testSession() session.Session {
	return session.Session{ID: "s", Role: "implement", Repos: []string{"owner/name"}}
}

func TestResolveAndCacheInstallID_FetchAndCache(t *testing.T) {
	configDir := eagerStateDir(t)
	seedPrimaryState(t, configDir, 0) // uncached

	var calledOwner, calledRepo string
	lookup := func(ctx context.Context, clientID string, ks keystore.Keystore, role, owner, repo string) (int64, error) {
		calledOwner, calledRepo = owner, repo
		return 777, nil
	}

	if err := resolveAndCacheInstallID(context.Background(), testSession(), lookup); err != nil {
		t.Fatalf("resolveAndCacheInstallID: %v", err)
	}
	if calledOwner != "owner" || calledRepo != "name" {
		t.Errorf("lookup called with %q/%q, want owner/name", calledOwner, calledRepo)
	}
	s, err := appsetup.ReadState(configDir)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if s.Primary.InstallationID != 777 {
		t.Errorf("cached id = %d, want 777", s.Primary.InstallationID)
	}
}

func TestResolveAndCacheInstallID_UnchangedNoRewrite(t *testing.T) {
	configDir := eagerStateDir(t)
	seedPrimaryState(t, configDir, 555)

	statePath := appsetup.StatePath(configDir)
	before, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	lookup := func(ctx context.Context, clientID string, ks keystore.Keystore, role, owner, repo string) (int64, error) {
		return 555, nil // same id -> no rewrite
	}
	if err := resolveAndCacheInstallID(context.Background(), testSession(), lookup); err != nil {
		t.Fatalf("resolveAndCacheInstallID: %v", err)
	}
	after, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("state.json was rewritten despite unchanged id (mtime %v -> %v)", before.ModTime(), after.ModTime())
	}
}

func TestResolveAndCacheInstallID_StaleRefresh(t *testing.T) {
	configDir := eagerStateDir(t)
	seedPrimaryState(t, configDir, 111) // stale cached id

	lookup := func(ctx context.Context, clientID string, ks keystore.Keystore, role, owner, repo string) (int64, error) {
		return 222, nil // rotated id
	}
	if err := resolveAndCacheInstallID(context.Background(), testSession(), lookup); err != nil {
		t.Fatalf("resolveAndCacheInstallID: %v", err)
	}
	s, _ := appsetup.ReadState(configDir)
	if s.Primary.InstallationID != 222 {
		t.Errorf("stale id not refreshed: got %d, want 222", s.Primary.InstallationID)
	}
}

func TestResolveAndCacheInstallID_404FailsLoud(t *testing.T) {
	configDir := eagerStateDir(t)
	seedPrimaryState(t, configDir, 0)

	lookup := func(ctx context.Context, clientID string, ks keystore.Keystore, role, owner, repo string) (int64, error) {
		return 0, githubapp.ErrAppNotInstalled
	}
	err := resolveAndCacheInstallID(context.Background(), testSession(), lookup)
	if err == nil {
		t.Fatal("expected fail-loud error on 404")
	}
	if !strings.Contains(err.Error(), "installations/new") {
		t.Errorf("404 error should carry the install deep-link: %v", err)
	}
	if !strings.Contains(err.Error(), "owner/name") {
		t.Errorf("404 error should name the repo: %v", err)
	}
	// state.json must be untouched (still 0).
	s, _ := appsetup.ReadState(configDir)
	if s.Primary.InstallationID != 0 {
		t.Errorf("state.json should be untouched on 404, got id=%d", s.Primary.InstallationID)
	}
}

func TestResolveAndCacheInstallID_TransientErrorWithCachedIDProceeds(t *testing.T) {
	configDir := eagerStateDir(t)
	seedPrimaryState(t, configDir, 333) // cached id available

	lookup := func(ctx context.Context, clientID string, ks keystore.Keystore, role, owner, repo string) (int64, error) {
		return 0, errors.New("github 503 transient")
	}
	// Non-404 error but a cached id exists -> proceed (nil error).
	if err := resolveAndCacheInstallID(context.Background(), testSession(), lookup); err != nil {
		t.Fatalf("should proceed on transient error with cached id, got: %v", err)
	}
	s, _ := appsetup.ReadState(configDir)
	if s.Primary.InstallationID != 333 {
		t.Errorf("cached id should be preserved, got %d", s.Primary.InstallationID)
	}
}

func TestResolveAndCacheInstallID_TransientErrorNoCacheFailsLoud(t *testing.T) {
	configDir := eagerStateDir(t)
	seedPrimaryState(t, configDir, 0) // no cached id

	lookup := func(ctx context.Context, clientID string, ks keystore.Keystore, role, owner, repo string) (int64, error) {
		return 0, errors.New("github 503 transient")
	}
	// Non-404 error AND no id to fall back to -> fail closed.
	if err := resolveAndCacheInstallID(context.Background(), testSession(), lookup); err == nil {
		t.Fatal("should fail closed on transient error with no cached id")
	}
}

func TestResolveAndCacheInstallID_EnvPathSkips(t *testing.T) {
	// Env path: all four vars set -> ResolveApp returns SourceEnv ->
	// no GET, no error.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("REIN_APP_CLIENT_ID", "Iv23li-env")
	t.Setenv("REIN_APP_PRIVATE_KEY_PATH", "/x.pem")
	t.Setenv("REIN_APP_INSTALLATION_ID", "99")
	t.Setenv("REIN_TEST_REPO_A", "owner/name")

	called := false
	lookup := func(ctx context.Context, clientID string, ks keystore.Keystore, role, owner, repo string) (int64, error) {
		called = true
		return 0, errors.New("should not be called on env path")
	}
	if err := resolveAndCacheInstallID(context.Background(), testSession(), lookup); err != nil {
		t.Fatalf("env path should be a no-op, got: %v", err)
	}
	if called {
		t.Error("lookup must not be called on the env path")
	}
}

func TestUnsetEnv(t *testing.T) {
	env := []string{"PATH=/bin", "GH_TOKEN=secret", "HOME=/home/x", "GH_TOKEN=dup"}
	got := unsetEnv(env, "GH_TOKEN")
	for _, kv := range got {
		if strings.HasPrefix(kv, "GH_TOKEN=") {
			t.Fatalf("GH_TOKEN not fully removed: %v", got)
		}
	}
	// Unrelated vars survive; all GH_TOKEN entries (incl. duplicates) gone.
	if len(got) != 2 {
		t.Fatalf("expected 2 surviving vars, got %d: %v", len(got), got)
	}
	// Unsetting an absent var is a no-op.
	if out := unsetEnv([]string{"A=1"}, "NOPE"); len(out) != 1 {
		t.Fatalf("unsetEnv of absent var should be a no-op, got %v", out)
	}
}

// runIDCharset is the load-bearing invariant: a run id becomes a filename
// (approvals/<id>.json, runs/<id>.json), so it must be filename-safe. This
// is exactly the base64.RawURLEncoding alphabet plus its no-padding rule.
var runIDCharset = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func TestNewRunID(t *testing.T) {
	a, err := newRunID()
	if err != nil {
		t.Fatalf("newRunID: %v", err)
	}
	if a == "" {
		t.Fatal("newRunID returned empty string")
	}
	if !runIDCharset.MatchString(a) {
		t.Errorf("run id %q is not filename-safe (want %s)", a, runIDCharset)
	}
	// 16 random bytes -> 22 chars of base64url (no padding).
	if len(a) != 22 {
		t.Errorf("run id %q has length %d, want 22", a, len(a))
	}

	b, err := newRunID()
	if err != nil {
		t.Fatalf("newRunID (second): %v", err)
	}
	if a == b {
		t.Errorf("two newRunID calls returned the same value %q; run ids must be unique", a)
	}
}

func TestParseRunIDFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"space form", []string{"--run-id", "abc123"}, "abc123"},
		{"equals form", []string{"--run-id=abc123"}, "abc123"},
		{"absent", []string{"--other", "x"}, ""},
		{"empty args", nil, ""},
		{"space form among others", []string{"foo", "--run-id", "xyz", "bar"}, "xyz"},
		{"equals form among others", []string{"foo", "--run-id=xyz", "bar"}, "xyz"},
		// --run-id as the final token with no following value -> empty.
		{"dangling flag no value", []string{"--run-id"}, ""},
		// Equals form with an empty value -> empty.
		{"equals empty value", []string{"--run-id="}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRunIDFlag(tc.args); got != tc.want {
				t.Errorf("parseRunIDFlag(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
