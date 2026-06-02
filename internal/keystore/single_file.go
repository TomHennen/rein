package keystore

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// SingleFileKeystore exposes one externally-owned file under one name.
// Read-only: Set/Delete return ErrReadOnly. Used by the Phase 0 env-var
// path (REIN_APP_PRIVATE_KEY_PATH) to satisfy the Keystore interface
// without coupling githubapp to a filesystem path.
//
// Unlike FileKeystore, this backend uses plain os.Stat + os.ReadFile
// (not O_NOFOLLOW). The PEM here is operator-owned — the developer may
// have legitimately placed it at a path that is a symlink into a
// secrets directory. FileKeystore's O_NOFOLLOW guard exists because
// rein owns those files and any symlink at the configured path is a
// red flag; that invariant doesn't apply to an operator-supplied path.
//
// uid + mode 0o077 enforcement still applies via verifyOwnership: the
// Phase 0 dev flow on this VM uses a 0600 PEM owned by the running
// user, and a chmod-loose or chown'd PEM is refused with the same
// clear error FileKeystore produces. See docs/init-manifest-design.md
// for the design intent.
type SingleFileKeystore struct {
	// Name is the only entry name Get/Fingerprint accept; other names
	// return ErrNotFound. Today every mint site uses "primary".
	Name string

	// Path is the absolute filesystem path to the PEM.
	Path string
}

// NewSingleFileKeystore constructs a backend that exposes path under
// the single entry name.
func NewSingleFileKeystore(name, path string) *SingleFileKeystore {
	return &SingleFileKeystore{Name: name, Path: path}
}

// Get returns the file bytes after verifying ownership (uid match) and
// mode (no group/other bits). Names other than k.Name return ErrNotFound.
//
// Single-fd to defeat TOCTOU: a Stat-then-Read pair lets an attacker
// swap a tight-mode symlink for a loose-mode target between the two
// path resolutions. Opening once and then fstat'ing+reading the same
// fd ensures we verify and consume the same inode. We deliberately do
// NOT pass O_NOFOLLOW (unlike FileKeystore): the file here is
// operator-owned and may legitimately be a symlink into a secrets
// directory; see the type doc above.
func (k *SingleFileKeystore) Get(name string) ([]byte, error) {
	if name != k.Name {
		return nil, ErrNotFound
	}
	f, err := os.Open(k.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open %s: %w", k.Path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("fstat %s: %w", k.Path, err)
	}
	if err := verifyOwnership(k.Path, info, os.Getuid()); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", k.Path, err)
	}
	return data, nil
}

// Set always returns ErrReadOnly. The Phase 0 env-var path manages its
// PEM out-of-band; mint code never writes here.
func (k *SingleFileKeystore) Set(name string, data []byte) error {
	return ErrReadOnly
}

// Delete always returns ErrReadOnly. Same rationale as Set.
func (k *SingleFileKeystore) Delete(name string) error {
	return ErrReadOnly
}

// Fingerprint returns the PKIX-SHA256 base64 fingerprint of the PEM,
// matching FileKeystore's format byte-for-byte for the same input.
func (k *SingleFileKeystore) Fingerprint(name string) (string, error) {
	data, err := k.Get(name)
	if err != nil {
		return "", err
	}
	return fingerprintPEM(name, data)
}
