// Package tokencache holds the on-disk shape of a cached installation
// token and helpers to read and atomically write the cache file.
//
// Shared by internal/broker (git read-token cache) and cmd/rein-gh (gh
// read-token cache) so the schema can evolve in one place. Phase 0 only;
// Phase 1's broker daemon holds tokens in memory and this package goes
// away.
package tokencache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Entry is the on-disk JSON shape. Permissions on the file are managed
// by Write; mode 0600 in a 0700 parent.
type Entry struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Valid reports whether the entry is still useful: non-empty and more
// than skew away from its expiry.
func (e Entry) Valid(skew time.Duration) bool {
	if e.Token == "" || e.ExpiresAt.IsZero() {
		return false
	}
	return time.Until(e.ExpiresAt) > skew
}

// Read parses the file at path into an Entry. Any error (missing file,
// malformed JSON) is reported as ok=false; the caller treats that as a
// cache miss. A non-fatal log line at the call site is appropriate for
// non-NotExist errors.
func Read(path string) (Entry, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(body, &e); err != nil {
		return Entry{}, fmt.Errorf("malformed cache: %w", err)
	}
	return e, nil
}

// Write atomically replaces the file at path with the JSON-marshalled
// entry. Mode 0600 from creation. The parent directory is created with
// 0700 if it doesn't exist.
//
// "Atomic" here means: a partial write is never visible to a reader;
// any reader either sees the prior contents or the new ones. Achieved
// via CreateTemp in the destination dir + chmod + rename. CreateTemp
// also avoids the fixed-tmp-name race when two writers run
// concurrently.
func Write(path string, e Entry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if rename succeeds
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}

// Delete removes the cache file. Missing-file is not an error.
func Delete(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
