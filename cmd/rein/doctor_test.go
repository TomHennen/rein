package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/appsetup"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/tokencache"
)

// clearAppEnv unsets the four REIN_APP_* vars so ResolveApp / the doctor
// checks take the state.json path rather than the env path, and points
// XDG_CONFIG_HOME at a temp dir so state.json + the keystore PEM live in an
// isolated, writable location. Returns that config dir.
func clearAppEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("REIN_APP_CLIENT_ID", "")
	t.Setenv("REIN_APP_PRIVATE_KEY_PATH", "")
	t.Setenv("REIN_APP_INSTALLATION_ID", "")
	t.Setenv("REIN_TEST_REPO_A", "")
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	dir, err := config.ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	return dir
}

// writeManifestState writes a manifest-phase state.json (primary App
// registered) with the given installation id into configDir.
func writeManifestState(t *testing.T, configDir string, installationID int64) {
	t.Helper()
	st := appsetup.State{
		Phase: appsetup.PhasePrimaryDone,
		Primary: &appsetup.AppRecord{
			ClientID:       "Iv23li-test",
			InstallationID: installationID,
		},
	}
	if err := appsetup.WriteState(configDir, st); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
}

func TestManagedPEMPath(t *testing.T) {
	t.Run("manifest phase returns primary.pem path", func(t *testing.T) {
		configDir := clearAppEnv(t)
		writeManifestState(t, configDir, 0)

		got, ok := managedPEMPath()
		if !ok {
			t.Fatalf("managedPEMPath ok=false on manifest-phase state; want true")
		}
		want := filepath.Join(configDir, config.AppKeystoreRole+".pem")
		if got != want {
			t.Errorf("managedPEMPath = %q; want %q", got, want)
		}
	})

	t.Run("absent state returns ok=false", func(t *testing.T) {
		clearAppEnv(t) // no state.json written
		if _, ok := managedPEMPath(); ok {
			t.Errorf("managedPEMPath ok=true with no state.json; want false")
		}
	})

	t.Run("non-manifest (managed_externally) returns ok=false", func(t *testing.T) {
		configDir := clearAppEnv(t)
		st := appsetup.State{
			Phase:  appsetup.PhaseManagedExternally,
			Source: appsetup.SourceEnv,
		}
		if err := appsetup.WriteState(configDir, st); err != nil {
			t.Fatalf("WriteState: %v", err)
		}
		if _, ok := managedPEMPath(); ok {
			t.Errorf("managedPEMPath ok=true on managed_externally state; want false")
		}
	})

	t.Run("corrupt state returns ok=false", func(t *testing.T) {
		configDir := clearAppEnv(t)
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(appsetup.StatePath(configDir), []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write corrupt state: %v", err)
		}
		if _, ok := managedPEMPath(); ok {
			t.Errorf("managedPEMPath ok=true on corrupt state.json; want false")
		}
	})
}

// TestCheckAppMint_InstallIDUncachedWarn covers the turnkey path where
// state.json is a manifest phase but installation_id is still 0 (App not yet
// installed on a repo, or first run). checkAppMint must return WARN and must
// NOT attempt a network mint — it has no installation id to mint with. The
// test makes no network call: with REIN_APP_* unset and a manifest-phase
// state carrying installation_id=0, ResolveApp returns SourceState with
// InstallationID==0 and checkAppMint short-circuits to warn before building a
// client.
func TestCheckAppMint_InstallIDUncachedWarn(t *testing.T) {
	configDir := clearAppEnv(t)
	writeManifestState(t, configDir, 0)

	res := checkAppMint()
	if res.status != statusWarn {
		t.Fatalf("checkAppMint status = %v; want statusWarn (install-id uncached, no mint)", res.status)
	}
}

// TestCheckAppKeyReadable_UsesManagedPEM covers the turnkey path where
// REIN_APP_PRIVATE_KEY_PATH is unset but state.json is a manifest phase:
// checkAppKeyReadable must fall back to the keystore-managed primary.pem
// (via managedPEMPath) and stat THAT file. We create a primary.pem with a
// loose mode and assert the check fails naming that managed path — proving
// it stat'd the managed file rather than reporting "env var unset".
func TestCheckAppKeyReadable_UsesManagedPEM(t *testing.T) {
	configDir := clearAppEnv(t)
	writeManifestState(t, configDir, 0)

	pemPath := filepath.Join(configDir, config.AppKeystoreRole+".pem")

	t.Run("managed pem readable with strict mode -> ok", func(t *testing.T) {
		if err := os.WriteFile(pemPath, []byte("-----BEGIN-----\n"), 0o600); err != nil {
			t.Fatalf("write pem: %v", err)
		}
		res := checkAppKeyReadable()
		if res.status != statusOK {
			t.Fatalf("checkAppKeyReadable status = %v (msg=%q); want statusOK", res.status, res.message)
		}
		if !strings.Contains(res.message, pemPath) {
			t.Errorf("message %q does not name managed PEM path %q", res.message, pemPath)
		}
	})

	t.Run("managed pem loose mode -> fail naming managed path", func(t *testing.T) {
		if err := os.WriteFile(pemPath, []byte("-----BEGIN-----\n"), 0o644); err != nil {
			t.Fatalf("write pem: %v", err)
		}
		// WriteFile only applies perm on create; the prior subtest already
		// created the file at 0600, so chmod explicitly to get loose mode.
		if err := os.Chmod(pemPath, 0o644); err != nil {
			t.Fatalf("chmod pem: %v", err)
		}
		res := checkAppKeyReadable()
		if res.status != statusFail {
			t.Fatalf("checkAppKeyReadable status = %v; want statusFail (loose mode)", res.status)
		}
		if !strings.Contains(res.message, pemPath) {
			t.Errorf("message %q does not name managed PEM path %q", res.message, pemPath)
		}
	})
}

// seedGhCache writes a per-scope gh-read cache entry (issue #95:
// gh-read-token-<tag>.json) for the given scope key under stateDir.
func seedGhCache(t *testing.T, stateDir, scopeKey string, e tokencache.Entry) {
	t.Helper()
	if err := tokencache.Write(ghsession.ReadCachePathForScope(stateDir, scopeKey), e); err != nil {
		t.Fatalf("seed gh cache %q: %v", scopeKey, err)
	}
}

// TestCheckGhShimCache_Aggregate pins the issue #95 glob/aggregate: with the
// gh-read cache now one file PER scope ceiling, checkGhShimCache globs them
// all and reports green iff ANY scope is warm, picking the LATEST expiry, and
// yellow (absent / all-expired) otherwise. A missing cache dir and a
// zero-match glob are handled without error.
func TestCheckGhShimCache_Aggregate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateDir, err := config.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}

	// (a) No cache dir at all -> Glob returns zero matches, no error -> absent.
	if res := checkGhShimCache(); res.status != statusWarn || !strings.Contains(res.message, "absent") {
		t.Fatalf("missing dir: status=%v msg=%q, want warn/absent", res.status, res.message)
	}

	// Seed a mix across distinct scopes: two valid (different expiries), one
	// expired, one corrupt. All filenames match the gh-read-token*.json glob.
	early := time.Now().Add(1 * time.Hour).Truncate(time.Second)
	latest := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	seedGhCache(t, stateDir, "scope-a", tokencache.Entry{Token: "t1", ExpiresAt: early})
	seedGhCache(t, stateDir, "scope-b", tokencache.Entry{Token: "t2", ExpiresAt: latest})
	seedGhCache(t, stateDir, "scope-c", tokencache.Entry{Token: "t3", ExpiresAt: time.Now().Add(-1 * time.Hour)})
	corrupt := filepath.Join(stateDir, "cache", "gh-read-token-corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	// (b) Mix -> green, exactly 2 valid, latest = the +3h expiry (not the +1h).
	res := checkGhShimCache()
	if res.status != statusOK {
		t.Fatalf("mix: status=%v msg=%q, want statusOK", res.status, res.message)
	}
	if !strings.Contains(res.message, "2 cached scope(s) valid") {
		t.Errorf("mix: message %q, want '2 cached scope(s) valid'", res.message)
	}
	if !strings.Contains(res.message, latest.Format(time.RFC3339)) {
		t.Errorf("mix: message %q, want latest expiry %s", res.message, latest.Format(time.RFC3339))
	}
	if strings.Contains(res.message, early.Format(time.RFC3339)) {
		t.Errorf("mix: message %q names the EARLIER expiry — latest selection is wrong", res.message)
	}

	// (c) Remove both valid scopes; only the expired + corrupt remain -> yellow.
	for _, key := range []string{"scope-a", "scope-b"} {
		if err := os.Remove(ghsession.ReadCachePathForScope(stateDir, key)); err != nil {
			t.Fatalf("remove %q: %v", key, err)
		}
	}
	res = checkGhShimCache()
	if res.status != statusWarn {
		t.Fatalf("all-invalid: status=%v msg=%q, want statusWarn", res.status, res.message)
	}
	if !strings.Contains(res.message, "2 cached scope(s) expired/unreadable") {
		t.Errorf("all-invalid: message %q, want '2 cached scope(s) expired/unreadable'", res.message)
	}
}
