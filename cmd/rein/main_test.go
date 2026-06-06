package main

import (
	"io"
	"log"
	"strings"
	"testing"

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
		RepoName:       "name",
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
