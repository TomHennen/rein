package appsetup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Phase values persisted in state.json's `phase` field.
const (
	PhasePrimaryDone       = "primary_done"
	PhaseAuditDone         = "audit_done"
	PhaseManagedExternally = "managed_externally"
)

// Source values for the managed_externally marker. Only "env" is
// written by CP5; "manifest" exists for symmetry with the post-manifest
// phases (which set Source on State itself, not on the marker).
const (
	SourceEnv      = "env"
	SourceManifest = "manifest"
)

// State is the on-disk shape of ~/.config/rein/state.json, 0600.
type State struct {
	Phase         string     `json:"phase"`
	Source        string     `json:"source,omitempty"`
	Primary       *AppRecord `json:"primary,omitempty"`
	Audit         *AppRecord `json:"audit,omitempty"`
	SchemaVersion int        `json:"schema_version"`
}

// AppRecord captures everything `rein doctor` and friends need to
// reconstruct the App without re-talking to GitHub. InstallationID is
// empty in CP5 (no install-poll yet — Stage 2 followup).
type AppRecord struct {
	Slug           string    `json:"slug,omitempty"`
	AppID          int64     `json:"app_id,omitempty"`
	ClientID       string    `json:"client_id"`
	InstallationID int64     `json:"installation_id,omitempty"`
	KeyFingerprint string    `json:"key_fingerprint,omitempty"`
	HTMLURL        string    `json:"html_url,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// StatePath returns the canonical state.json path
// (<config-dir>/state.json). The configDir comes from
// internal/config.ConfigDir() at the call site; passed in to keep this
// package decoupled from XDG resolution.
func StatePath(configDir string) string {
	return filepath.Join(configDir, "state.json")
}

// ReadState reads and JSON-unmarshals StatePath(configDir). Returns
// (zero-State, fs.ErrNotExist) wrapping the original NotExist when the
// file is absent, so callers can distinguish "fresh install" from
// "corrupt file." Malformed JSON returns a descriptive error and the
// caller must NOT auto-delete — the operator is the only one allowed
// to remove state.json.
func ReadState(configDir string) (State, error) {
	p := StatePath(configDir)
	body, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{}, err
		}
		return State{}, fmt.Errorf("read %s: %w", p, err)
	}
	var s State
	if err := json.Unmarshal(body, &s); err != nil {
		return State{}, fmt.Errorf("parse %s: %w (refusing to auto-delete; inspect or remove manually)", p, err)
	}
	return s, nil
}

// WriteState atomically replaces StatePath(configDir) with s, mode
// 0600, using the design's 9-step sequence (CreateTemp -> Chmod ->
// Write -> Sync -> Close -> Rename -> dirSync). The PEM and state.json
// share this pattern but each package owns its own implementation —
// the duplication is intentional so the schema can evolve per-package
// without coupling.
func WriteState(configDir string, s State) error {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", configDir, err)
	}
	if s.SchemaVersion == 0 {
		s.SchemaVersion = 1
	}
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	return atomicWriteFile(StatePath(configDir), body, 0o600)
}

// atomicWriteFile mirrors keystore.FileKeystore.Set's atomic sequence
// for non-keystore files (currently just state.json). Kept package-
// private; if a third caller appears, lift to a shared helper.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp into place: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open parent dir for fsync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync parent dir: %w", err)
	}
	return nil
}
