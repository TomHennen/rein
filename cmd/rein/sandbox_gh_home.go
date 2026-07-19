// Rein-owned, per-run GH_CONFIG_DIR overlay for sandboxed gh (the gh twin of the
// CLAUDE_CONFIG_DIR overlay, #94). Host ~/.config/gh is hidden in-sandbox by
// nono's default-deny fs (deny_credentials + nothing grants it), so gh reading it
// EACCESes and refuses to start. gh is instead repointed at this rein-owned,
// writable overlay via the profile's GH_CONFIG_DIR set_var.
//
// gh will not send ANY request unless it believes it is authenticated, so the
// overlay scaffolds a hosts.yml with a deliberately-INVALID placeholder token: gh
// then sends the request and rein's proxy OVERWRITES the Authorization header
// with the real short-lived token (downstream of the sandbox — the agent never
// sees it; see proxy.go "overwrite (covers a dummy GH_TOKEN)"). The placeholder
// is not a credential; the developer's real hosts.yml + PAT stay hidden.
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

// prepareGhOverlay creates a per-run, rein-owned, agent-WRITABLE GH_CONFIG_DIR
// under parent and scaffolds a hosts.yml with the placeholder token. Returns the
// overlay's absolute path. The dir is created 0700 and hardened to the same bar
// as the claude overlay (not a symlink, user-owned, tight mode) before anything
// is written. Host-side, before launch. Per-run: the caller removes it at teardown.
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
