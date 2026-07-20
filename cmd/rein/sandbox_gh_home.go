// Rein-owned, per-run GH_CONFIG_DIR overlay for sandboxed gh (the gh twin of the
// CLAUDE_CONFIG_DIR overlay, #94). nono's default-deny fs hides the host
// ~/.config/gh, so gh EACCESes reading it and refuses to start; the profile's
// GH_CONFIG_DIR set_var repoints it at this writable overlay.
//
// gh won't send a request unless it believes it's authenticated, so the overlay's
// hosts.yml carries a deliberately-invalid placeholder token; rein's proxy then
// overwrites the Authorization header with the real short-lived token downstream of
// the sandbox (the agent never sees it). The developer's real hosts.yml + PAT stay
// hidden.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// ghStubToken is the non-secret placeholder gh oauth_token scaffolded into the
// overlay's hosts.yml. Never used for auth — rein's proxy injects the real token
// downstream — it only makes gh consider itself logged in so it sends requests.
// Ported from srt's stubGHToken.
const ghStubToken = "x-access-token-rein-sandbox-stub"

// prepareGhOverlay creates a per-run, rein-owned, agent-writable GH_CONFIG_DIR
// under parent with a placeholder hosts.yml and returns its absolute path. The dir
// is 0700 and hardened (not a symlink, user-owned, tight mode) before any write;
// the caller removes it at teardown.
//
// Unlike the claude overlay it does NOT symlink-harden the parent: the only file
// written is the fixed non-secret placeholder, so there's no token a redirected
// parent could exfiltrate — MkdirTemp's fresh 0700 dir under a sticky temp root
// suffices.
func prepareGhOverlay(parent string) (string, error) {
	overlay, err := os.MkdirTemp(parent, "rein-gh-")
	if err != nil {
		return "", fmt.Errorf("create gh overlay: %w", err)
	}
	if err := assertTightUserDir(overlay); err != nil {
		return "", err
	}
	hosts := fmt.Sprintf("github.com:\n    oauth_token: %s\n    user: rein-sandbox\n    git_protocol: https\n", ghStubToken)
	if err := writeOverlayFile(filepath.Join(overlay, "hosts.yml"), []byte(hosts)); err != nil {
		return "", fmt.Errorf("scaffold gh hosts.yml: %w", err)
	}
	return overlay, nil
}
