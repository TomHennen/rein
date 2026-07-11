package main

import (
	"context"
	"errors"
	"io/fs"
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

// fakeProber stubs installProber. repoFn answers the per-repo coverage probes;
// slug/slugErr answer AppSlug, which ONLY the env path's refusal deep-link
// calls (the state path takes the slug from state.json and must never make that
// GET — slugCalls is what asserts that, and that the happy path never pays for
// an error message it doesn't need).
type fakeProber struct {
	repoFn    func(ctx context.Context, owner, repo string) (int64, error)
	slug      string
	slugErr   error
	slugCalls int
}

func (f *fakeProber) RepoInstallationID(ctx context.Context, owner, repo string) (int64, error) {
	return f.repoFn(ctx, owner, repo)
}

func (f *fakeProber) AppSlug(ctx context.Context) (string, error) {
	f.slugCalls++
	if f.slugErr != nil {
		return "", f.slugErr
	}
	return f.slug, nil
}

// factory adapts a fakeProber to the installProberFactory seam.
func (f *fakeProber) factory() installProberFactory {
	return func(clientID string, ks keystore.Keystore, roleName string) (installProber, error) {
		return f, nil
	}
}

// proberFn is the common case: only the repo probes matter.
func proberFn(fn func(ctx context.Context, owner, repo string) (int64, error)) installProberFactory {
	return (&fakeProber{repoFn: fn, slug: "rein-test"}).factory()
}

func TestResolveAndCacheInstallID_FetchAndCache(t *testing.T) {
	configDir := eagerStateDir(t)
	seedPrimaryState(t, configDir, 0) // uncached

	var calledOwner, calledRepo string
	lookup := proberFn(func(ctx context.Context, owner, repo string) (int64, error) {
		calledOwner, calledRepo = owner, repo
		return 777, nil
	})

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

	lookup := proberFn(func(ctx context.Context, owner, repo string) (int64, error) {
		return 555, nil // same id -> no rewrite
	})
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

	lookup := proberFn(func(ctx context.Context, owner, repo string) (int64, error) {
		return 222, nil // rotated id
	})
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

	f := &fakeProber{slug: "should-not-be-asked", repoFn: func(ctx context.Context, owner, repo string) (int64, error) {
		return 0, githubapp.ErrAppNotInstalled
	}}
	err := resolveAndCacheInstallID(context.Background(), testSession(), f.factory())
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
	// The state path already HAS the slug (state.json); it must not spend a
	// GET /app to rediscover it.
	if f.slugCalls != 0 {
		t.Errorf("state path must not call AppSlug (called %d times)", f.slugCalls)
	}
	if !strings.Contains(err.Error(), "rein-test") {
		t.Errorf("state-path refusal should use state.json's slug: %v", err)
	}
}

// multiRepoSession is a same-owner two-repo session, the shape
// session.Validate explicitly supports (issue #44 D4: coverage of EVERY
// repo must be verified at launch, not just Repos[0]).
func multiRepoSession() session.Session {
	return session.Session{ID: "s", Role: "implement", Repos: []string{"owner/name", "owner/other"}}
}

func TestResolveAndCacheInstallID_MultiRepo404OnSecondRepoFailsLoud(t *testing.T) {
	configDir := eagerStateDir(t)
	seedPrimaryState(t, configDir, 555)

	lookup := proberFn(func(ctx context.Context, owner, repo string) (int64, error) {
		if repo == "other" {
			return 0, githubapp.ErrAppNotInstalled // Repos[1] not in the installation
		}
		return 555, nil
	})
	err := resolveAndCacheInstallID(context.Background(), multiRepoSession(), lookup)
	if err == nil {
		t.Fatal("expected fail-loud error when a non-first session repo is uncovered")
	}
	if !strings.Contains(err.Error(), "owner/other") {
		t.Errorf("error should name the uncovered repo owner/other: %v", err)
	}
	if !strings.Contains(err.Error(), "installations/new") {
		t.Errorf("error should carry the install deep-link: %v", err)
	}
}

func TestResolveAndCacheInstallID_MultiRepoAllCoveredPasses(t *testing.T) {
	configDir := eagerStateDir(t)
	seedPrimaryState(t, configDir, 0) // uncached

	var probed []string
	lookup := proberFn(func(ctx context.Context, owner, repo string) (int64, error) {
		probed = append(probed, owner+"/"+repo)
		return 777, nil
	})
	if err := resolveAndCacheInstallID(context.Background(), multiRepoSession(), lookup); err != nil {
		t.Fatalf("all-covered multi-repo session should pass: %v", err)
	}
	if len(probed) != 2 || probed[0] != "owner/name" || probed[1] != "owner/other" {
		t.Errorf("every session repo must be probed, got %v", probed)
	}
	s, err := appsetup.ReadState(configDir)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if s.Primary.InstallationID != 777 {
		t.Errorf("cached id = %d, want 777", s.Primary.InstallationID)
	}
}

func TestResolveAndCacheInstallID_MultiRepoMismatchedIDsFailsLoud(t *testing.T) {
	configDir := eagerStateDir(t)
	seedPrimaryState(t, configDir, 0)

	lookup := proberFn(func(ctx context.Context, owner, repo string) (int64, error) {
		if repo == "other" {
			return 222, nil
		}
		return 111, nil
	})
	err := resolveAndCacheInstallID(context.Background(), multiRepoSession(), lookup)
	if err == nil {
		t.Fatal("expected fail-loud error when session repos resolve to different installation ids")
	}
	if !strings.Contains(err.Error(), "111") || !strings.Contains(err.Error(), "222") {
		t.Errorf("mismatch error should carry both ids: %v", err)
	}
	// state.json must be untouched (still 0) — no id was authoritative.
	s, _ := appsetup.ReadState(configDir)
	if s.Primary.InstallationID != 0 {
		t.Errorf("state.json should be untouched on id mismatch, got id=%d", s.Primary.InstallationID)
	}
}

func TestResolveAndCacheInstallID_MultiRepoTransientOnOneRepoProceeds(t *testing.T) {
	configDir := eagerStateDir(t)
	seedPrimaryState(t, configDir, 0) // uncached: the resolved repo supplies the id

	lookup := proberFn(func(ctx context.Context, owner, repo string) (int64, error) {
		if repo == "other" {
			return 0, errors.New("github 503 transient")
		}
		return 444, nil
	})
	// A transient (non-404) blip on one repo must not ground the session
	// when another repo resolved an id — mirrors the single-repo
	// transient-with-cached-id behavior.
	if err := resolveAndCacheInstallID(context.Background(), multiRepoSession(), lookup); err != nil {
		t.Fatalf("transient error on one repo with another resolved should proceed, got: %v", err)
	}
	s, _ := appsetup.ReadState(configDir)
	if s.Primary.InstallationID != 444 {
		t.Errorf("resolved id should be cached, got %d, want 444", s.Primary.InstallationID)
	}
}

func TestResolveAndCacheInstallID_TransientErrorWithCachedIDProceeds(t *testing.T) {
	configDir := eagerStateDir(t)
	seedPrimaryState(t, configDir, 333) // cached id available

	lookup := proberFn(func(ctx context.Context, owner, repo string) (int64, error) {
		return 0, errors.New("github 503 transient")
	})
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

	lookup := proberFn(func(ctx context.Context, owner, repo string) (int64, error) {
		return 0, errors.New("github 503 transient")
	})
	// Non-404 error AND no id to fall back to -> fail closed.
	if err := resolveAndCacheInstallID(context.Background(), testSession(), lookup); err == nil {
		t.Fatal("should fail closed on transient error with no cached id")
	}
}

// eagerEnvPath sets all four REIN_APP_* vars so ResolveApp takes the env path,
// and points XDG_CONFIG_HOME at an empty dir (no state.json — the env path must
// neither need one nor create one). The env id is 99. The PEM path is never
// stat'd: LoadAppConfig doesn't touch it, and the keystore is only read inside
// the injected lookup, which the tests stub.
//
// Issue #68: the env path used to early-return before probing, so an
// installation that did not COVER a session repo produced a successful launch
// and a TM-G8 placeholder inside the agent. It now runs the same per-repo
// verification as the state path; only the state.json caching is state-only.
func eagerEnvPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("REIN_APP_CLIENT_ID", "Iv23li-env")
	t.Setenv("REIN_APP_PRIVATE_KEY_PATH", "/x.pem")
	t.Setenv("REIN_APP_INSTALLATION_ID", "99")
	t.Setenv("REIN_TEST_REPO_A", "owner/name")
	return filepath.Join(dir, "rein")
}

// uncoveredSecondRepo probes clean on Repos[0] and 404s on Repos[1] — an
// installation that exists but does not COVER the whole session.
func uncoveredSecondRepo(ctx context.Context, owner, repo string) (int64, error) {
	if repo == "other" {
		return 0, githubapp.ErrAppNotInstalled
	}
	return 99, nil
}

func TestResolveAndCacheInstallID_EnvPathUncoveredRepoRefused(t *testing.T) {
	eagerEnvPath(t)

	// The env config carries an installation id — but the installation does
	// not cover Repos[1]. Presence of an id is NOT coverage: refuse. The
	// deep-link is the App-SPECIFIC install page, whose slug the env path has
	// to ask GitHub for (AppSlug) since it has no state.json to read it from.
	f := &fakeProber{repoFn: uncoveredSecondRepo, slug: "rein-test"}
	err := resolveAndCacheInstallID(context.Background(), multiRepoSession(), f.factory())
	if err == nil {
		t.Fatal("env path with an uncovered session repo must be refused (issue #68)")
	}
	if !strings.Contains(err.Error(), "owner/other") {
		t.Errorf("error should name the uncovered repo owner/other: %v", err)
	}
	if !strings.Contains(err.Error(), "https://github.com/apps/rein-test/installations/new") {
		t.Errorf("error should carry the App-specific install deep-link: %v", err)
	}
}

func TestResolveAndCacheInstallID_EnvPathSlugLookupFailureStillRefuses(t *testing.T) {
	eagerEnvPath(t)

	// The slug GET fails. The refusal must STILL fire — a cosmetic lookup
	// failure may degrade the deep-link but must never soften, replace, or
	// swallow the coverage refusal (fail closed).
	f := &fakeProber{repoFn: uncoveredSecondRepo, slugErr: errors.New("github 503")}
	err := resolveAndCacheInstallID(context.Background(), multiRepoSession(), f.factory())
	if err == nil {
		t.Fatal("uncovered repo must be refused even when the slug lookup fails")
	}
	if !strings.Contains(err.Error(), "owner/other") {
		t.Errorf("error should still name the uncovered repo: %v", err)
	}
	if !strings.Contains(err.Error(), "https://github.com/settings/installations") {
		t.Errorf("error should fall back to the generic installations link: %v", err)
	}
	if strings.Contains(err.Error(), "503") {
		t.Errorf("the slug-lookup error must not leak into the refusal: %v", err)
	}
}

func TestResolveAndCacheInstallID_EnvPathAllCoveredProceedsNoStateWrite(t *testing.T) {
	configDir := eagerEnvPath(t)

	var probed []string
	f := &fakeProber{slug: "rein-test", repoFn: func(ctx context.Context, owner, repo string) (int64, error) {
		probed = append(probed, owner+"/"+repo)
		return 99, nil // agrees with REIN_APP_INSTALLATION_ID
	}}
	if err := resolveAndCacheInstallID(context.Background(), multiRepoSession(), f.factory()); err != nil {
		t.Fatalf("all-covered env-path session should proceed: %v", err)
	}
	if len(probed) != 2 || probed[0] != "owner/name" || probed[1] != "owner/other" {
		t.Errorf("every session repo must be probed on the env path too, got %v", probed)
	}
	// The env path owns no state.json and must not create one.
	if _, err := os.Stat(appsetup.StatePath(configDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("env path must not write state.json (stat err = %v)", err)
	}
	// The slug is only needed to BUILD a refusal. A covered session must not
	// pay for a GET /app it never uses.
	if f.slugCalls != 0 {
		t.Errorf("AppSlug called %d times on the happy path; want 0", f.slugCalls)
	}
}

func TestResolveAndCacheInstallID_EnvPathIDMismatchFailsLoud(t *testing.T) {
	eagerEnvPath(t)

	lookup := proberFn(func(ctx context.Context, owner, repo string) (int64, error) {
		return 12345, nil // GitHub disagrees with REIN_APP_INSTALLATION_ID=99
	})
	err := resolveAndCacheInstallID(context.Background(), testSession(), lookup)
	if err == nil {
		t.Fatal("env id contradicting GitHub's id must fail loud")
	}
	if !strings.Contains(err.Error(), "99") || !strings.Contains(err.Error(), "12345") {
		t.Errorf("mismatch error should carry both the env id and the probed id: %v", err)
	}
	if !strings.Contains(err.Error(), "REIN_APP_INSTALLATION_ID") {
		t.Errorf("mismatch error should name the offending env var: %v", err)
	}
}

func TestResolveAndCacheInstallID_EnvPathTransientProceeds(t *testing.T) {
	eagerEnvPath(t)

	lookup := proberFn(func(ctx context.Context, owner, repo string) (int64, error) {
		return 0, errors.New("github 503 transient")
	})
	// Transient (non-404) failure with an id in hand (the env var) -> warn and
	// proceed, exactly as the state path does with a cached id. A GitHub blip
	// must not ground a session the env id would have served.
	if err := resolveAndCacheInstallID(context.Background(), testSession(), lookup); err != nil {
		t.Fatalf("env path should proceed on a transient probe error: %v", err)
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
