package tokencache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadMissing(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

func TestReadMalformed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(p, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := Read(p)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected malformed-cache error, got %v", err)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "tok.json")
	want := Entry{Token: "ghs_xyz", ExpiresAt: time.Now().Add(45 * time.Minute).Truncate(time.Second)}
	if err := Write(p, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
	got, err := Read(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Token != want.Token {
		t.Errorf("token = %q, want %q", got.Token, want.Token)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("expires = %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
}

func TestWriteAtomicTempCleanup(t *testing.T) {
	// After a successful Write, no .tmp file should linger in the dir.
	dir := t.TempDir()
	p := filepath.Join(dir, "tok.json")
	if err := Write(p, Entry{Token: "x", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if name != "tok.json" {
			t.Errorf("unexpected leftover file in dir: %s", name)
		}
	}
}

func TestValidSkew(t *testing.T) {
	near := Entry{Token: "x", ExpiresAt: time.Now().Add(10 * time.Second)}
	if near.Valid(30 * time.Second) {
		t.Error("expected near-expiry to be invalid under 30s skew")
	}
	far := Entry{Token: "x", ExpiresAt: time.Now().Add(45 * time.Minute)}
	if !far.Valid(30 * time.Second) {
		t.Error("expected far-expiry to be valid under 30s skew")
	}
	empty := Entry{}
	if empty.Valid(0) {
		t.Error("zero-value entry must not be valid")
	}
	notoken := Entry{ExpiresAt: time.Now().Add(time.Hour)}
	if notoken.Valid(0) {
		t.Error("entry with no token must not be valid")
	}
}

func TestDeleteMissingIsNotAnError(t *testing.T) {
	if err := Delete(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Errorf("Delete of missing should not error, got %v", err)
	}
}

// TestJSONShapeCompatibility ensures the on-disk JSON shape matches what
// broker and rein-gh have been writing in CP3/CP3.6. Files from those
// versions must still load cleanly after this extraction.
func TestJSONShapeCompatibility(t *testing.T) {
	body := []byte(`{"token":"ghs_legacy","expires_at":"2026-12-31T23:59:59Z"}`)
	p := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	e, err := Read(p)
	if err != nil {
		t.Fatalf("read legacy: %v", err)
	}
	if e.Token != "ghs_legacy" {
		t.Errorf("token = %q", e.Token)
	}

	// Also: round-trip a current Entry and verify it produces the same
	// schema legacy code would expect.
	out := Entry{Token: "t", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	body, err = json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"token":"t","expires_at":"2030-01-01T00:00:00Z"}`
	if string(body) != want {
		t.Errorf("JSON = %s, want %s", string(body), want)
	}
}
