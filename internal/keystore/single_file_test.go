package keystore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTightPEM seeds a 0600 PEM at <dir>/app.pem and returns the path.
// Mirrors the env-var Phase 0 flow: an operator-managed PEM on disk,
// owned by the running uid with strict mode bits.
func writeTightPEM(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(p, genTestPEM(t), 0o600); err != nil {
		t.Fatalf("seed PEM: %v", err)
	}
	return p
}

func TestSingleFileKeystore_Get_Roundtrip(t *testing.T) {
	p := writeTightPEM(t, t.TempDir())
	k := NewSingleFileKeystore("primary", p)
	got, err := k.Get("primary")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Get returned %d bytes; want fixture (%d bytes)", len(got), len(want))
	}
}

func TestSingleFileKeystore_Get_WrongNameReturnsNotFound(t *testing.T) {
	p := writeTightPEM(t, t.TempDir())
	k := NewSingleFileKeystore("primary", p)
	_, err := k.Get("audit")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(\"audit\") = %v, want ErrNotFound", err)
	}
}

func TestSingleFileKeystore_Get_MissingFileReturnsNotFound(t *testing.T) {
	k := NewSingleFileKeystore("primary", filepath.Join(t.TempDir(), "no-such.pem"))
	_, err := k.Get("primary")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on missing file = %v, want ErrNotFound", err)
	}
}

func TestSingleFileKeystore_Get_RefusesLooseMode(t *testing.T) {
	p := writeTightPEM(t, t.TempDir())
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatalf("chmod loose: %v", err)
	}
	k := NewSingleFileKeystore("primary", p)
	_, err := k.Get("primary")
	if err == nil {
		t.Fatal("expected loose-mode refusal, got nil")
	}
	if !strings.Contains(err.Error(), "group/other") {
		t.Errorf("err = %q, want substring %q", err.Error(), "group/other")
	}
}

func TestSingleFileKeystore_Set_ReadOnly(t *testing.T) {
	p := writeTightPEM(t, t.TempDir())
	k := NewSingleFileKeystore("primary", p)
	if err := k.Set("primary", []byte("attempt")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Set = %v, want ErrReadOnly", err)
	}
}

func TestSingleFileKeystore_Delete_ReadOnly(t *testing.T) {
	p := writeTightPEM(t, t.TempDir())
	k := NewSingleFileKeystore("primary", p)
	if err := k.Delete("primary"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Delete = %v, want ErrReadOnly", err)
	}
}

func TestSingleFileKeystore_Fingerprint_MatchesFileKeystore(t *testing.T) {
	// Both backends should produce byte-identical fingerprints for the
	// same PEM — the format is the operator-visible identifier in
	// GitHub's App-settings UI, and a divergence would silently break
	// fingerprint-based "is this the registered key?" checks.
	pemBytes := genTestPEM(t)

	dir := t.TempDir()
	pPath := filepath.Join(dir, "single.pem")
	if err := os.WriteFile(pPath, pemBytes, 0o600); err != nil {
		t.Fatalf("seed single: %v", err)
	}
	single := NewSingleFileKeystore("primary", pPath)
	fpSingle, err := single.Fingerprint("primary")
	if err != nil {
		t.Fatalf("SingleFileKeystore.Fingerprint: %v", err)
	}

	fk := NewFileKeystore(t.TempDir())
	if err := fk.Set("primary", pemBytes); err != nil {
		t.Fatalf("FileKeystore.Set: %v", err)
	}
	fpFile, err := fk.Fingerprint("primary")
	if err != nil {
		t.Fatalf("FileKeystore.Fingerprint: %v", err)
	}

	if fpSingle != fpFile {
		t.Errorf("fingerprint mismatch: single=%q file=%q", fpSingle, fpFile)
	}
}

func TestSingleFileKeystore_Fingerprint_NotFound(t *testing.T) {
	k := NewSingleFileKeystore("primary", filepath.Join(t.TempDir(), "no-such.pem"))
	_, err := k.Fingerprint("primary")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Fingerprint missing = %v, want ErrNotFound", err)
	}
}
