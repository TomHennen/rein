package proxy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CheckPlacement fails closed unless socketPath sits OUTSIDE every directory in
// forbidden. This enforces design §5.3's hard invariant: the proxy socket is a
// capability, and srt bind-mounts the working directory into the sandbox — if
// the socket ever lands under a bound path, the in-sandbox agent gets direct,
// unmediated access to the capability, bypassing the classifier and write
// approval. CP3 passes the srt bind-mounts as forbidden; this is unit-tested
// now so the check is proven before it gates a live launch.
//
// The comparison is on cleaned absolute paths: socketPath is inside a forbidden
// dir D if it equals D or is under D/. Symlinks are resolved where the paths
// exist so a symlinked socket dir can't smuggle the socket under a bound path.
func CheckPlacement(socketPath string, forbidden []string) error {
	sock, err := ResolveAbs(socketPath)
	if err != nil {
		return fmt.Errorf("proxy: resolve socket path %q: %w", socketPath, err)
	}
	for _, dir := range forbidden {
		d, err := ResolveAbs(dir)
		if err != nil {
			// A forbidden dir we can't resolve is treated as still-forbidden by
			// path prefix on the un-resolved (but cleaned/abs) form — fail
			// closed rather than skip it.
			d = cleanAbs(dir)
		}
		if pathWithin(sock, d) {
			return fmt.Errorf("proxy: socket %q is inside forbidden directory %q (srt bind-mount would expose the capability to the sandbox; design §5.3)", socketPath, dir)
		}
	}
	return nil
}

// pathWithin reports whether child is equal to parent or nested under it. Both
// are expected to be cleaned absolute paths. It compares path segments (not raw
// string prefixes) so "/a/bc" is NOT considered within "/a/b".
func pathWithin(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// Nested iff the relative path does not start by walking OUT of parent.
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ResolveAbs returns the cleaned, absolute, symlink-resolved path. When the
// leaf doesn't exist yet (the socket isn't bound), it resolves the longest
// existing ancestor and re-appends the remainder, so a symlinked parent dir is
// still followed. Exported because it is the ONE symlink-resolution primitive
// shared by every sandbox path comparison: the socket placement check here, and
// run_nono's working-tree / mapped-checkout resolution before it grants those
// paths writable via nono --allow. A symlinked path must never smuggle a
// widening under (or a capability into) a
// checked directory just because the comparison ran on the unresolved form.
func ResolveAbs(p string) (string, error) {
	abs := cleanAbs(p)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	// Leaf may not exist yet: resolve the deepest existing ancestor.
	dir := filepath.Dir(abs)
	base := filepath.Base(abs)
	for dir != filepath.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, base), nil
		}
		base = filepath.Join(filepath.Base(dir), base)
		dir = filepath.Dir(dir)
	}
	return abs, nil
}

func cleanAbs(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// Listen creates the per-run proxy listener at socketPath after the placement
// check passes. It enforces the design §5.2/§5.3 socket invariants: a
// filesystem unix socket (never abstract namespace), a 0700 parent dir, and a
// 0600 socket. A stale socket file from a crashed run is removed first.
func Listen(socketPath string, forbidden []string) (*net.UnixListener, error) {
	// Reject abstract-namespace sockets explicitly (design §5.3: filesystem
	// socket only). On Linux a leading "@" or NUL means the abstract namespace,
	// which has no path on disk and bypasses filesystem permissions entirely,
	// so the 0700-dir / 0600-socket capability gating wouldn't apply. Fail
	// closed here rather than relying on a downstream chmod error.
	if socketPath == "" || socketPath[0] == '@' || strings.ContainsRune(socketPath, '\x00') {
		return nil, fmt.Errorf("proxy: socket path %q is not a filesystem path (abstract-namespace sockets are forbidden; design §5.3)", socketPath)
	}
	if err := CheckPlacement(socketPath, forbidden); err != nil {
		return nil, err
	}
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("proxy: create socket dir: %w", err)
	}
	// MkdirAll won't tighten a pre-existing looser dir; the dir is the primary
	// access control for the capability, so force 0700.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("proxy: tighten socket dir to 0700: %w", err)
	}
	// A leftover socket file from a crashed run blocks the bind; remove it —
	// but ONLY if it's stale. Probe first: if something is still listening
	// there, another run owns this path, and unlinking it would silently hijack
	// that session's socket (design §5.2 socket = session identity). Fail
	// closed instead.
	if _, err := os.Lstat(socketPath); err == nil {
		if conn, derr := net.DialTimeout("unix", socketPath, 500*time.Millisecond); derr == nil {
			conn.Close()
			return nil, fmt.Errorf("proxy: %s is already in use by a live listener; refusing to take it over", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("proxy: remove stale socket: %w", err)
		}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("proxy: listen on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("proxy: tighten socket to 0600: %w", err)
	}
	return ln, nil
}
