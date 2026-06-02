// Package keystore is rein's pluggable secret-on-disk abstraction.
//
// CP5 ships two backends:
//
//   - FileKeystore: 0600 PEM files under ~/.config/rein/, owned and
//     managed by rein (manifest-flow output).
//   - SingleFileKeystore: a read-only wrapper around an
//     operator-managed PEM at a configured path (used to bridge the
//     Phase 0 REIN_APP_PRIVATE_KEY_PATH env var into this interface
//     without coupling githubapp to a filesystem path).
//
// The interface exists so Phase 1's daemon backend (memory-cached) and
// Phase 1/2's biometric backend (LAContext + age on macOS) can swap in
// without churning the token-mint code paths. See design §"Keystore
// interface — defined now, file-backed only" and research §5.
//
// Mint-path coverage (CP5 post-keystore-refactor): the token-minting
// code in internal/githubapp reads PEMs exclusively via Keystore.Get.
// Both backends apply the same uid + mode 0o077 ownership check before
// returning bytes; loose-mode or wrong-owner PEMs hard-fail the mint
// rather than silently succeed. This matches CLAUDE.md hard constraint
// #6 ("All private-key reads MUST go through internal/keystore.Keystore").
package keystore

import "errors"

// Keystore is the surface every backend implements. Names are
// caller-opaque strings (e.g. "primary", "audit"); the backend maps
// each name to a single bytestring entry.
type Keystore interface {
	// Get returns the bytes stored under name. File-backed
	// implementations MUST verify ownership and mode
	// (uid == os.Getuid(), mode & 0o077 == 0). Other backends may
	// surface a biometric prompt here.
	Get(name string) ([]byte, error)

	// Set stores data under name atomically. Existing entries are
	// overwritten. Mode 0600 on file backends.
	Set(name string, data []byte) error

	// Delete removes the entry. Missing entries are not errors.
	Delete(name string) error

	// Fingerprint returns a stable identifier for the stored PEM,
	// suitable for cross-machine identification and for detecting
	// "the local PEM is no longer the registered key." CP5's
	// FileKeystore implements this as base64(SHA-256(PKIX(public-key))),
	// which is the format we expect to match GitHub's App-settings UI
	// display — verified at the manual smoke test in CP5's gate.
	Fingerprint(name string) (string, error)
}

// ErrNotFound is returned by Get/Fingerprint when the named entry
// does not exist. Callers distinguish "not configured yet" from
// "configured but unreadable."
var ErrNotFound = errors.New("keystore: entry not found")

// ErrReadOnly is returned by Set and Delete on backends that wrap an
// externally-owned file (today: SingleFileKeystore, used to bridge the
// Phase 0 REIN_APP_PRIVATE_KEY_PATH env var into the Keystore
// interface). The mint path never calls Set/Delete; this sentinel exists
// so a caller that accidentally does gets a clear error rather than a
// silent or platform-specific syscall error.
var ErrReadOnly = errors.New("keystore: backend is read-only")
