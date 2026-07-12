// Git-identity wiring for the sandboxed run (CP4). Resolves the
// non-impersonating GIT_AUTHOR_*/GIT_COMMITTER_* identity and writes the
// rein-managed GIT_CONFIG_GLOBAL that keeps the developer's ~/.gitconfig out of
// the sandbox. See internal/gitidentity for the resolution logic + fallbacks.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/gitidentity"
	"github.com/TomHennen/rein/internal/keystore"
)

// gitIdentityTimeout caps the two identity lookups (GET /app + GET /users/…).
// Cached after the first successful launch, so this only bites once. Generous
// enough for a cold App-JWT mint + two round-trips; fail-open on timeout to a
// non-impersonating fallback (never blocks the launch).
const gitIdentityTimeout = 10 * time.Second

// hostGitName returns `git config --get user.name` from rein's own (host)
// environment — the developer's real name, run OUTSIDE the sandbox. Empty on
// any error (git absent, name unset): the identity resolver falls back to the
// owner login, then a branded default.
func hostGitName() string {
	cmd := exec.Command("git", "config", "--get", "user.name")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resolveGitIdentity builds the non-impersonating identity for the sandbox.
// clientID/ks authenticate the App-JWT /app slug lookup; ownerLogin is the
// name fallback; knownSlug (from state.json, if any) can skip the /app call.
// Configurable via REIN_GIT_AUTHOR_TEMPLATE (name, "{name}" placeholder) and
// REIN_GIT_AUTHOR_EMAIL (verbatim email override).
func resolveGitIdentity(clientID string, ks keystore.Keystore, ownerLogin, knownSlug, cachePath string, logger *log.Logger) gitidentity.Identity {
	apiBase := os.Getenv("REIN_GITHUB_API_BASE")

	lookupSlug := func(ctx context.Context) (string, error) {
		c, err := githubapp.NewAppClient(clientID, ks, config.AppKeystoreRole, apiBase)
		if err != nil {
			return "", err
		}
		return c.AppSlug(ctx)
	}
	lookupBotID := func(ctx context.Context, botLogin string) (int64, error) {
		return githubapp.BotUserID(ctx, apiBase, botLogin)
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitIdentityTimeout)
	defer cancel()
	id := gitidentity.Resolve(ctx, gitidentity.Params{
		HostGitName:   hostGitName(),
		OwnerLogin:    ownerLogin,
		NameTemplate:  os.Getenv("REIN_GIT_AUTHOR_TEMPLATE"),
		EmailOverride: os.Getenv("REIN_GIT_AUTHOR_EMAIL"),
		KnownSlug:     knownSlug,
		AppIdentity:   clientID, // invalidates a cached email if the App changes
		CachePath:     cachePath,
		LookupSlug:    lookupSlug,
		LookupBotID:   lookupBotID,
		Logger:        logger,
	})
	return id
}

// writeManagedGitConfig writes the per-run gitconfig that GIT_CONFIG_GLOBAL
// points at. It carries the identity (belt-and-suspenders for tools that read
// git config rather than the env vars) and, by being the global config, stops
// git from reading the developer's ~/.gitconfig in-sandbox.
//
// Tradeoff (v1): the developer's aliases/editor/signing prefs are NOT carried
// into the sandbox. Acceptable — the sandbox runs an agent, not the human — and
// noted so a future version that layers a sanitized subset is a deliberate add,
// not a regression. Mode 0644: it must be readable in-sandbox (like the CA
// bundle); it holds no secret (a bot noreply address + a public name).
func writeManagedGitConfig(path string, id gitidentity.Identity) error {
	// core.excludesFile hides srt's injected agent-env dotfiles (below) from
	// `git status` so they don't read as untracked changes in the repo working
	// tree — noise the agent trips on ("N uncommitted changes"), and a real
	// footgun: an agent that runs `git add -A` would otherwise stage srt's
	// dotfiles into a commit. Global excludes (not the repo's .gitignore or
	// .git/info/exclude) so it applies without mutating the user's repo. Written
	// next to this config, in the same runTmp that's readable in-sandbox (#102).
	excludes := filepath.Join(filepath.Dir(path), "rein-sandbox.gitignore")
	if err := writeSrtDotfileExcludes(excludes); err != nil {
		return err
	}
	body := "" +
		"# rein-managed git config for this sandboxed run (CP4).\n" +
		"# GIT_CONFIG_GLOBAL points here so the sandbox does NOT read the\n" +
		"# developer's ~/.gitconfig (which would leak their email + credential-\n" +
		"# helper config and make agent commits author as the developer).\n" +
		"# GIT_AUTHOR_*/GIT_COMMITTER_* env vars are the robust authorship lever\n" +
		"# (they override this file); this identity is the config-level twin.\n" +
		"# Tradeoff: the developer's aliases/editor/signing prefs are not carried\n" +
		"# into the sandbox (v1) — the sandbox runs an agent, not the human.\n" +
		"[user]\n" +
		"\tname = " + id.Name + "\n" +
		"\temail = " + id.Email + "\n" +
		"[core]\n" +
		"\texcludesFile = " + excludes + "\n"
	return os.WriteFile(path, []byte(body), 0o644)
}

// srtInjectedDotfiles are the agent-environment dotfiles srt drops into the
// agent's cwd (= rein's repo working tree). Observed on the pinned srt
// (internal/srt.PinnedVersion 0.0.63); revisit on a pin bump — rein re-verifies
// srt then, so this list is checked alongside. Named exactly (not a broad
// `.*` glob) so a repo's OWN untracked dotfiles (a new .gitignore, .github, …)
// are NOT hidden — only srt's are.
var srtInjectedDotfiles = []string{
	".bash_profile", ".bashrc", ".profile", ".zprofile", ".zshrc",
	".gitconfig", ".gitmodules",
	".mcp.json", ".ripgreprc",
	".claude", ".idea", ".vscode", // no trailing slash: match whether file or dir
}

// writeSrtDotfileExcludes writes the gitignore-format exclude list. Leading
// slash anchors each to the repo root (where srt injects them), so a legitimately
// tracked/created file of the same name deeper in the tree is unaffected.
func writeSrtDotfileExcludes(path string) error {
	var b strings.Builder
	b.WriteString("# rein: srt injects these agent-env dotfiles into the working\n")
	b.WriteString("# tree's root; hide them from git so they aren't noise or an\n")
	b.WriteString("# accidental `git add -A` commit. (#102; mirrors srt 0.0.63.)\n")
	for _, f := range srtInjectedDotfiles {
		b.WriteString("/" + f + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// ownerFromRepo extracts the owner login from an "owner/name" repo string —
// the App-installation owner (single-owner), used as the git-name fallback.
func ownerFromRepo(ownerSlashName string) string {
	owner, _, ok := strings.Cut(ownerSlashName, "/")
	if !ok {
		return ""
	}
	return owner
}

// gitIdentityCachePath is the bot-email cache file. Lives in ConfigDir alongside
// the App key/CA (which is denyRead'd in-sandbox); resolution happens OUTSIDE
// the sandbox at launch, so the deny-read does not impede it.
func gitIdentityCachePath() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir for git-identity cache: %w", err)
	}
	return dir + "/bot-identity.json", nil
}
