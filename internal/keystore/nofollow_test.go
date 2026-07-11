package keystore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestFileKeystore_Get_RefusesSymlink exercises the O_NOFOLLOW symlink-swap
// defense (docs/design.md:571,729; CLAUDE.md hard-constraint #6). The
// conformance audit (issue #44) found the guard implemented at file.go:59 but
// with NO test: repo-wide, no keystore test creates a symlink, so a regression
// dropping O_NOFOLLOW would compile and pass the entire suite while silently
// re-opening the symlink-swap TOCTOU. This is the missing coverage.
func TestFileKeystore_Get_RefusesSymlink(t *testing.T) {
	k := NewFileKeystore(t.TempDir())

	// A real, same-uid, 0600 PEM the symlink points at. Placed OUTSIDE the
	// keystore dir so the keystore's own path is only ever the symlink.
	target := filepath.Join(t.TempDir(), "real.pem")
	if err := os.WriteFile(target, genTestPEM(t), 0o600); err != nil {
		t.Fatalf("write target PEM: %v", err)
	}

	// Swap the keystore's expected path for a symlink to that readable PEM.
	link := k.path("primary")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatalf("mkdir keystore dir: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Precondition: the target is readable via a symlink-following read, so
	// the ONLY reason Get can refuse is the O_NOFOLLOW pin — this proves the
	// test exercises the pin, not a permission accident.
	if _, err := os.ReadFile(link); err != nil {
		t.Fatalf("precondition: symlinked PEM should be readable via a following read: %v", err)
	}

	_, err := k.Get("primary")
	if err == nil {
		t.Fatal("Get must refuse a symlinked PEM (O_NOFOLLOW), got nil error")
	}
	// It must be a HARD refusal, not ErrNotFound: a symlink is a red flag,
	// not an absent key, so callers fail closed rather than fall back.
	if errors.Is(err, ErrNotFound) {
		t.Errorf("symlink refusal must not masquerade as ErrNotFound: %v", err)
	}
}
