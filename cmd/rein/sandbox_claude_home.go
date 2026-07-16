// Rein-owned persistent CLAUDE_CONFIG_DIR overlay for sandboxed claude runs
// (issue #94). Host ~/.claude and ~/.claude.json are DEFAULT-DENIED in-sandbox
// (credentialDenyReadPaths); claude is instead repointed at this rein-owned
// overlay via CLAUDE_CONFIG_DIR (internal/srt/env.go). The overlay is bound
// read-WRITE via ExtraAllowWrite and PERSISTS across runs, so claude sessions
// resume — while the host's real ~/.claude cross-project history stays hidden.
//
// Everything here runs HOST-SIDE, before the in-sandbox deny is applied. rein
// seeds ONLY .credentials.json (copied fresh from the host every launch — the
// OAuth token lives ~6h) and authors its OWN minimal settings.json. It does NOT
// seed .claude.json (the overlay regenerates it — seeding would leak host
// project history) and NEVER copies the sandbox's rotated creds back to host.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"syscall"

	"github.com/TomHennen/rein/internal/config"
)

// sandboxClaudeSettings is the rein-authored minimal settings.json placed in the
// overlay. rein reads NOTHING from claude's settings today (the only settings.json
// in the Go tree is srt's OWN), so this stays deliberately minimal — host
// user-settings are NOT copied (spike: repointing CLAUDE_CONFIG_DIR does not merge
// them anyway). The one setting is the sandbox's explicit permission posture:
// skipDangerousModePermissionPrompt suppresses the startup confirmation that
// rein's --dangerously-skip-permissions launch would otherwise trigger and that
// would hang a headless/autonomous run. The sandbox IS the security boundary, so
// claude's own permission prompt is redundant here.
const sandboxClaudeSettings = `{
  "skipDangerousModePermissionPrompt": true
}
`

// prepareClaudeOverlay creates/refreshes the rein-owned CLAUDE_CONFIG_DIR overlay
// and returns its absolute path. On EVERY launch it (1) creates the overlay 0700
// (it holds the OAuth token), fail-closed if the path is a symlink or not
// user-owned; (2) seeds .credentials.json fresh from the host so the sandboxed
// claude authenticates; (3) authors rein's own minimal settings.json.
//
// Absent host creds is NOT an error: rein guards GitHub credentials, not claude
// auth. The run proceeds with an unauthenticated overlay (claude reports "Not
// logged in") — honest, not a silent degrade of any rein security control.
func prepareClaudeOverlay(logger *log.Logger, home string) (string, error) {
	overlay, err := config.SandboxClaudeHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve claude overlay dir: %w", err)
	}
	if err := os.MkdirAll(overlay, 0o700); err != nil {
		return "", fmt.Errorf("create claude overlay %q: %w", overlay, err)
	}
	if err := assertTightUserDir(overlay); err != nil {
		return "", err
	}
	if err := seedClaudeCredentials(logger, home, overlay); err != nil {
		return "", err
	}
	if err := writeOverlayFile(filepath.Join(overlay, "settings.json"), []byte(sandboxClaudeSettings)); err != nil {
		return "", fmt.Errorf("author claude overlay settings.json: %w", err)
	}
	return overlay, nil
}

// seedClaudeCredentials copies the host's ~/.claude/.credentials.json into the
// overlay, host-side, on every launch (token freshness). Both the read and the
// write refuse to follow a symlink and require user ownership — the overlay holds
// the OAuth token, so match the keystore's security bar (uid + O_NOFOLLOW) even
// though this is not a PEM. Absent host creds => skip (see prepareClaudeOverlay).
func seedClaudeCredentials(logger *log.Logger, home, overlay string) error {
	src := filepath.Join(home, ".claude", ".credentials.json")
	data, err := readUserFileNoFollow(src)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Printf("claude overlay: host %q absent — not seeding creds (claude will be unauthenticated in-sandbox)", src)
			return nil
		}
		return fmt.Errorf("read host claude credentials %q: %w", src, err)
	}
	if err := writeOverlayFile(filepath.Join(overlay, ".credentials.json"), data); err != nil {
		return fmt.Errorf("seed claude overlay credentials: %w", err)
	}
	return nil
}

// assertTightUserDir fails closed if dir is a symlink, is not a directory, is not
// owned by the current uid, or is group/other-accessible (mode & 0o077 != 0). The
// overlay holds the OAuth token; a tampered (symlinked/loosened/foreign-owned)
// overlay must abort the launch rather than seed a token into it.
func assertTightUserDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat claude overlay %q: %w", dir, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("claude overlay %q is a symlink; refusing to seed a credential into it (fail closed)", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("claude overlay %q is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("claude overlay %q is owned by uid %d, not the current user %d (fail closed)", dir, st.Uid, os.Getuid())
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("claude overlay %q has mode %o; want 0700 (holds the OAuth token) (fail closed)", dir, fi.Mode().Perm())
	}
	return nil
}

// readUserFileNoFollow reads a regular, user-owned file without following a
// symlink at the final component (O_NOFOLLOW). A symlink there fails the open.
func readUserFileNoFollow(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("%q is owned by uid %d, not the current user %d (fail closed)", path, st.Uid, os.Getuid())
	}
	return io.ReadAll(f)
}

// writeOverlayFile writes data to an overlay file 0600, truncating any prior
// content and refusing to follow a symlink at the final component (O_NOFOLLOW
// defeats a symlink-swap of the target). Chmod pins 0600 even if the file
// pre-existed with looser perms (OpenFile's mode only applies on create).
func writeOverlayFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, werr := f.Write(data); werr != nil {
		f.Close()
		return werr
	}
	if cerr := f.Chmod(0o600); cerr != nil {
		f.Close()
		return cerr
	}
	return f.Close()
}
