package keystore

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// FileKeystore stores entries as plain 0600 files in a 0700 directory.
// The mapping is name -> "<dir>/<name>.pem".
//
// On read, ownership and mode are verified strictly: a PEM that's been
// chown'd to another uid or chmod'd to anything with group/other bits
// is refused with a clear error that names the file path, the observed
// values, and the expected values. This mirrors SSH/age's loose-perm
// refusal and is the file-backed analogue of the biometric prompt the
// Phase 1/2 backend will use.
type FileKeystore struct {
	Dir string
}

// NewFileKeystore returns a FileKeystore rooted at dir. The dir is
// not created here; CP1 creates ~/.config/rein at 0700.
func NewFileKeystore(dir string) *FileKeystore {
	return &FileKeystore{Dir: dir}
}

func (k *FileKeystore) path(name string) string {
	return filepath.Join(k.Dir, name+".pem")
}

// PathOf returns the on-disk path FileKeystore uses for a given name.
// Exposed so callers that need to refer to the file (for stat-only
// checks or for human-facing messages) don't have to duplicate the
// layout. Mint paths should still go through Get — calling Open
// directly skips the ownership + mode verification.
func (k *FileKeystore) PathOf(name string) string {
	return k.path(name)
}

// Get implements Keystore.Get.
//
// Uses a single fd for Stat + Read so a symlink-swap or chown-race
// between the two cannot defeat the ownership check. O_NOFOLLOW
// additionally refuses to follow a symlink at the final path
// component — a PEM at the configured location must be a regular
// file owned by this uid; a symlink (even one we'd otherwise have
// permission to read) is a red flag.
func (k *FileKeystore) Get(name string) ([]byte, error) {
	p := k.path(name)
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("fstat %s: %w", p, err)
	}
	if err := verifyOwnership(p, info, os.Getuid()); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	return data, nil
}

// Set implements Keystore.Set with the design §"Key storage timing"
// 9-step atomic sequence: CreateTemp -> Chmod(0600) -> Write ->
// Sync(data) -> Close -> Rename -> dirSync.
func (k *FileKeystore) Set(name string, data []byte) error {
	if err := os.MkdirAll(k.Dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", k.Dir, err)
	}
	final := k.path(name)
	tmp, err := os.CreateTemp(k.Dir, "."+name+".pem-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", k.Dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	// Flush data blocks from page cache to disk BEFORE rename so a
	// crash between rename and the OS's lazy data flush can't leave
	// the renamed target pointing at zero bytes.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("rename temp into place: %w", err)
	}
	// Flush the dirent update so a crash here can't leave the file
	// orphaned by inode but absent by name.
	if err := dirSync(k.Dir); err != nil {
		return fmt.Errorf("fsync parent dir: %w", err)
	}
	return nil
}

// Delete implements Keystore.Delete. Missing-file is not an error.
func (k *FileKeystore) Delete(name string) error {
	if err := os.Remove(k.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", k.path(name), err)
	}
	return nil
}

// Fingerprint implements Keystore.Fingerprint.
func (k *FileKeystore) Fingerprint(name string) (string, error) {
	data, err := k.Get(name)
	if err != nil {
		return "", err
	}
	return fingerprintPEM(name, data)
}

// fingerprintPEM decodes the first PEM block in data, parses it as
// PKCS#1 or PKCS#8 RSA, marshals the PKIX public key, then base64-
// encodes the SHA-256 digest. The format is the expected match for
// GitHub's App-settings UI display; verify at the manual smoke test.
//
// Shared by FileKeystore.Fingerprint and SingleFileKeystore.Fingerprint
// so both backends produce byte-identical fingerprints for the same PEM.
// name is included in error messages only — it's not part of the digest.
func fingerprintPEM(name string, data []byte) (string, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("fingerprint %s: no PEM block found", name)
	}
	var pub interface{}
	if key, perr := x509.ParsePKCS1PrivateKey(block.Bytes); perr == nil {
		pub = &key.PublicKey
	} else if key, perr := x509.ParsePKCS8PrivateKey(block.Bytes); perr == nil {
		// Both *rsa.PrivateKey and *ecdsa.PrivateKey carry a .Public()
		// method; using the interface keeps the helper format-agnostic.
		if signer, ok := key.(interface{ Public() interface{} }); ok {
			pub = signer.Public()
		} else {
			return "", fmt.Errorf("fingerprint %s: PKCS8 key has no Public(): %T", name, key)
		}
	} else {
		return "", fmt.Errorf("fingerprint %s: parse private key (tried PKCS1, PKCS8): %w", name, perr)
	}
	pkix, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("fingerprint %s: marshal PKIX: %w", name, err)
	}
	sum := sha256.Sum256(pkix)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// verifyOwnership refuses entries owned by a different uid or with
// any group/other permission bits set. Extracted as a helper so the
// pure logic can be unit-tested without requiring multi-user setup.
func verifyOwnership(path string, info os.FileInfo, wantUID int) error {
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return fmt.Errorf("keystore: %s mode %#o has group/other bits set; expected mode & 0077 == 0 (chmod 600 %s)", path, mode, path)
	}
	// Unix-only ownership check. On non-Unix the syscall.Stat_t cast
	// fails and we fall through to a "can't verify" return so the
	// caller still treats the entry as usable. (The mode check above
	// is platform-agnostic and remains the primary defence.)
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(stat.Uid) != wantUID {
		return fmt.Errorf("keystore: %s uid %d does not match this process uid %d; refusing to read", path, stat.Uid, wantUID)
	}
	return nil
}

// dirSync opens dir read-only, fsyncs it, closes. Required after Rename
// so the dirent update is durable.
func dirSync(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
