package appsetup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadState_Missing(t *testing.T) {
	_, err := ReadState(t.TempDir())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("got %v, want fs.ErrNotExist", err)
	}
}

func TestReadState_Malformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := ReadState(dir)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestWriteState_RoundTrip_PrimaryDone(t *testing.T) {
	dir := t.TempDir()
	want := State{
		Phase:  PhasePrimaryDone,
		Source: SourceManifest,
		Primary: &AppRecord{
			Slug:           "rein-primary-deadbeef00",
			AppID:          12345,
			ClientID:       "Iv23liabc",
			KeyFingerprint: "fp1=",
			HTMLURL:        "https://github.com/apps/rein-primary-deadbeef00",
			CreatedAt:      time.Date(2026, 5, 30, 14, 0, 0, 0, time.UTC),
		},
	}
	if err := WriteState(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(StatePath(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %#o, want 0600", mode)
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Phase != want.Phase || got.Source != want.Source || got.SchemaVersion != 1 {
		t.Errorf("got %+v, want phase=%q source=%q schema_version=1", got, want.Phase, want.Source)
	}
	if got.Primary == nil || got.Primary.Slug != want.Primary.Slug {
		t.Errorf("primary mismatch: %+v", got.Primary)
	}
	if got.Audit != nil {
		t.Errorf("audit should be nil, got %+v", got.Audit)
	}
}

func TestWriteState_RoundTrip_ManagedExternally(t *testing.T) {
	dir := t.TempDir()
	want := State{
		Phase:  PhaseManagedExternally,
		Source: SourceEnv,
		Primary: &AppRecord{
			ClientID:       "Iv23liabc",
			InstallationID: 99887766,
			CreatedAt:      time.Date(2026, 5, 30, 14, 0, 0, 0, time.UTC),
		},
	}
	if err := WriteState(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Phase != PhaseManagedExternally {
		t.Errorf("phase = %q", got.Phase)
	}
	if got.Primary == nil || got.Primary.InstallationID != 99887766 {
		t.Errorf("primary.installation_id = %+v", got.Primary)
	}
	// Marker records shouldn't carry a fingerprint — the env points to
	// a user-managed PEM and rein doesn't claim ownership of it.
	if got.Primary != nil && got.Primary.KeyFingerprint != "" {
		t.Errorf("marker primary should have empty fingerprint, got %q", got.Primary.KeyFingerprint)
	}
}

func TestWriteState_AtomicNoLeftovers(t *testing.T) {
	dir := t.TempDir()
	if err := WriteState(dir, State{Phase: PhaseAuditDone}); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("leftover file in config dir: %s", e.Name())
		}
	}
}
