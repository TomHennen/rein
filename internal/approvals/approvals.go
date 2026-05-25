// Package approvals records that a human has approved write access for
// a session. One approval covers all writes within the session for the
// approval's TTL (4 hours default — matches design §4.2.2's
// `default_read_ttl` for the implement role).
//
// # Why a single approval, not per-write
//
// PLAN.md CP5's original framing prompted per-push. That turned out to
// match neither design §2.2 (where the prompt is at session START to
// establish scope) nor reasonable UX (a productive agent session can
// involve many small pushes; prompting on each is wearing). The
// approval record makes the prompt a session-start ceremony instead.
//
// # Invalidation
//
// An approval invalidates whenever ANY of the session's identity-
// shaping fields change: ID, Role, Repos, or Issue. The Signature
// hashes those fields; on each ConfirmWrite the caller compares the
// stored signature to the current session's signature. A mismatch is
// treated as "no approval" and forces a re-prompt.
//
// Time-based invalidation: approvals carry ExpiresAt; reads past the
// expiry are misses.
//
// # Escape hatches
//
// User wants to force a re-prompt without waiting for the TTL:
//   - Delete the file at Path(stateDir)
//   - Edit dev-session.yaml (changes the signature)
//   - Use `rein approval clear` (planned subcommand)
package approvals

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/session"
)

// Record is the on-disk shape of an approval.
type Record struct {
	// Signature is the hash of the session fields the approval covers.
	// Mismatch with the current session's signature invalidates the
	// approval — the operator changed scope and must re-approve.
	Signature string `json:"signature"`

	// SessionID is recorded for log/audit clarity; the actual identity
	// check is via Signature (which includes ID).
	SessionID string `json:"session_id"`

	// ApprovedAt is when the human typed the issue number.
	ApprovedAt time.Time `json:"approved_at"`

	// ExpiresAt is when this approval stops being honored. Reads past
	// this time return a miss and force re-prompting.
	ExpiresAt time.Time `json:"expires_at"`
}

// SignatureOf computes the deterministic signature for a session. Any
// change to ID, Role, Repos, or Issue produces a different signature.
// Repo ordering is normalized via sort so a re-ordered list doesn't
// invalidate an existing approval.
//
// Created is intentionally NOT in the signature: the approval is keyed
// to the SESSION (operator's intent), not to its file mtime.
func SignatureOf(s session.Session) string {
	repos := append([]string{}, s.Repos...)
	sort.Strings(repos)
	h := sha256.New()
	fmt.Fprintf(h, "id=%s\n", s.ID)
	fmt.Fprintf(h, "role=%s\n", s.Role)
	fmt.Fprintf(h, "repos=%s\n", strings.Join(repos, ","))
	fmt.Fprintf(h, "issue=%d\n", s.Issue)
	return hex.EncodeToString(h.Sum(nil))
}

// Valid reports whether rec is a usable approval right now: it covers
// the expected signature and hasn't expired.
func Valid(rec Record, expected string, now time.Time) bool {
	if rec.Signature == "" || expected == "" {
		return false
	}
	if rec.Signature != expected {
		return false
	}
	return now.Before(rec.ExpiresAt)
}

// Path returns the canonical on-disk location for the approval file
// within stateDir.
func Path(stateDir string) string {
	return filepath.Join(stateDir, "approval.json")
}

// Read returns the approval record at path. Missing-file is reported
// as a distinguishable error (os.ErrNotExist via errors.Is).
func Read(path string) (Record, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(body, &rec); err != nil {
		return Record{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return rec, nil
}

// Write atomically writes the record (0600 file in 0700 parent dir).
func Write(path string, rec Record) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir approval: %w", err)
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "approval.json.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Clear removes the approval file. Missing-file is not an error.
func Clear(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
