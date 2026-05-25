package approvals

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/session"
)

func TestSignatureOf_Stable(t *testing.T) {
	s := session.Session{
		ID:    "sess_1",
		Role:  "implement",
		Repos: []string{"o/a", "o/b"},
		Issue: 7,
	}
	a := SignatureOf(s)
	b := SignatureOf(s)
	if a != b {
		t.Errorf("signature not stable: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Errorf("expected sha256 hex (64 chars), got len=%d", len(a))
	}
}

func TestSignatureOf_ChangesPerField(t *testing.T) {
	base := session.Session{ID: "x", Role: "implement", Repos: []string{"o/a"}, Issue: 1}
	cases := []struct {
		name   string
		mutate func(s *session.Session)
	}{
		{"id changes", func(s *session.Session) { s.ID = "y" }},
		{"role changes", func(s *session.Session) { s.Role = "scan" }},
		{"repos add", func(s *session.Session) { s.Repos = []string{"o/a", "o/b"} }},
		{"repos different", func(s *session.Session) { s.Repos = []string{"o/c"} }},
		{"issue changes", func(s *session.Session) { s.Issue = 2 }},
	}
	baseSig := SignatureOf(base)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mutate(&s)
			if SignatureOf(s) == baseSig {
				t.Errorf("expected signature change, got identical")
			}
		})
	}
}

func TestSignatureOf_RepoOrderInsensitive(t *testing.T) {
	a := SignatureOf(session.Session{ID: "x", Role: "r", Repos: []string{"o/a", "o/b"}, Issue: 1})
	b := SignatureOf(session.Session{ID: "x", Role: "r", Repos: []string{"o/b", "o/a"}, Issue: 1})
	if a != b {
		t.Errorf("repo order should not affect signature: %q vs %q", a, b)
	}
}

func TestSignatureOf_CreatedIgnored(t *testing.T) {
	// Re-loading the session file (which updates Created via the
	// filesystem) shouldn't invalidate an approval.
	a := SignatureOf(session.Session{ID: "x", Role: "r", Repos: []string{"o/a"}, Issue: 1, Created: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)})
	b := SignatureOf(session.Session{ID: "x", Role: "r", Repos: []string{"o/a"}, Issue: 1, Created: time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)})
	if a != b {
		t.Errorf("Created should not affect signature: %q vs %q", a, b)
	}
}

func TestValid(t *testing.T) {
	now := time.Now()
	rec := Record{
		Signature:  "abc",
		ApprovedAt: now.Add(-10 * time.Minute),
		ExpiresAt:  now.Add(20 * time.Minute),
	}
	if !Valid(rec, "abc", now) {
		t.Error("valid record should return true")
	}
	if Valid(rec, "xyz", now) {
		t.Error("mismatched signature should return false")
	}
	expired := rec
	expired.ExpiresAt = now.Add(-1 * time.Minute)
	if Valid(expired, "abc", now) {
		t.Error("expired record should return false")
	}
	empty := Record{}
	if Valid(empty, "abc", now) {
		t.Error("empty signature should return false")
	}
	if Valid(rec, "", now) {
		t.Error("empty expected should return false")
	}
}

func TestWriteRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	want := Record{
		Signature:  "abc123",
		SessionID:  "sess_x",
		ApprovedAt: time.Now().Truncate(time.Second).UTC(),
		ExpiresAt:  time.Now().Add(4 * time.Hour).Truncate(time.Second).UTC(),
	}
	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Signature != want.Signature || got.SessionID != want.SessionID || !got.ApprovedAt.Equal(want.ApprovedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, want)
	}
}

func TestRead_Missing(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

func TestClear(t *testing.T) {
	t.Run("removes existing", func(t *testing.T) {
		dir := t.TempDir()
		path := Path(dir)
		if err := Write(path, Record{Signature: "x"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := Clear(path); err != nil {
			t.Errorf("Clear: %v", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Error("file should be gone after Clear")
		}
	})
	t.Run("missing is not an error", func(t *testing.T) {
		if err := Clear(filepath.Join(t.TempDir(), "absent.json")); err != nil {
			t.Errorf("Clear of missing should not error, got %v", err)
		}
	})
}
