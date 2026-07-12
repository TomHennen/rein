// Git-hardening for writable checkouts bound into the sandbox (issue #64).
//
// Why this design (references — the security rationale travels with the code):
//
//   - Git HOOKS are arbitrary code run automatically by ordinary git ops
//     (commit/merge/checkout): git-scm.com/docs/githooks. So .git/hooks is a
//     host-code-execution surface the agent must not be able to write.
//   - .git/CONFIG is ALSO code execution, not just hooks — core.fsmonitor,
//     core.pager, core.sshCommand, core.editor, alias.*=!cmd all run programs:
//     git-scm.com/docs/git-config; corroborated by CVE-2023-29007 (config
//     injection). So .git/config must be read-only too.
//   - "Opening an untrusted .git runs code" is a recognized class with real
//     CVEs + vendor mitigations: CVE-2022-24765 / safe.directory (GitHub's
//     advisory spells out the ACE consequence), and CVE-2026-26268 (Cursor AI
//     IDE, git-hook ACE) as the AI-agent analog. Sandboxed agent-runners already
//     treat .git specially (OpenAI Codex protects it) — direct prior art.
//   - Pinning .git as a bind MOUNTPOINT makes it un-renameable: rename(2)
//     returns EBUSY for "a directory in use by the system"
//     (man7.org/linux/man-pages/man2/rename.2.html). That is what defeats the
//     rename-parent evasion below.
//
// The rename-parent evasion specifically (`mv .git .aside` frees the ro-bind
// path, then rebuild a malicious .git) is THIN in public docs — no source states
// it directly — so rein PROVES it empirically: the escape attempt is a live
// regression test (tests/interactive) showing the mv fails EBUSY once .git is
// pinned. Treat that test, not a citation, as the evidence for this step.
package srt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvSandboxAllowUnhardenedGit is the LOUD kill switch for the git-hardening
// fail-closed guard (the rename-parent escape family; see Params.WritableGitDirs).
// rein pins each writable checkout's top-level `.git` and deny-writes its
// hooks/config, but that hardening does NOT cover two repo shapes:
//
//   - a checkout with SUBMODULES: each `.git/modules/<name>` is a real gitdir
//     with its own hooks/config that stay writable — a confirmed host-code-
//     execution surface a prompt-injected agent can plant into; and
//   - a LINKED worktree (`git worktree add`), where `.git` is a FILE
//     (`gitdir: …`): there is no `<tree>/.git` dir to pin, the `.git` file
//     itself is writable (the agent can repoint it at an evil gitdir), and the
//     real hooks live in the shared common gitdir, possibly outside the tree.
//
// Because those surfaces would run AS THE DEVELOPER ON THE HOST, rein FAILS
// CLOSED (refuses the launch) when a writable checkout has either shape rather
// than binding it writable with a hole (hard-constraint #3). A truthy value
// here ("1"/"true"/"yes"/"on") downgrades the refusal to a loud warning and
// proceeds with the partial (top-level-only) hardening — the operator's
// explicit, informed opt-in. The alternatives that need no opt-in: remove the
// tree from the session's `worktrees:` map (or unset REIN_WORKTREES) so it is
// cloned ephemerally instead of bound writable.
const EnvSandboxAllowUnhardenedGit = "REIN_SANDBOX_ALLOW_UNHARDENED_GIT"

// AllowUnhardenedGitFromEnv reports whether the value opts IN to binding a
// not-fully-hardenable checkout writable. Only an explicit truthy value opts
// in; anything else (unset, empty, "0", "false", garbage) keeps the fail-closed
// default.
func AllowUnhardenedGitFromEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// GitHardeningGap names a writable checkout whose `.git` cannot be fully
// hardened against the rename-parent host-exec escape, and why.
type GitHardeningGap struct {
	// Tree is the checkout's working-tree path (as passed in).
	Tree string
	// Reason is a short, human-readable explanation of the residual surface.
	Reason string
}

// AssessGitHardening inspects each writable checkout and returns a gap for every
// one whose top-level-`.git` pinning would leave a host-code-execution surface
// exposed. An empty result means every tree is fully hardenable by pinning
// `<tree>/.git` + deny-writing its hooks/config (what Build does).
//
// It is deliberately conservative (fail closed): a `.git` it cannot classify
// (stat error other than not-exist) is reported as a gap. A tree with no `.git`
// at all is NOT a gap here — it is not a git checkout, so there is no `.git`
// exec surface to protect (worktree.Resolve only maps real checkouts; this
// tolerates a caller that passes a non-repo).
//
// KNOWN RESIDUAL — deliberate scope boundary, not an oversight (issue #76): this
// covers each checkout's TOP-LEVEL `.git`. It does NOT catch a BURIED bare repo
// planted ELSEWHERE in the writable tree (a nested `.git`-like dir the agent
// commits, whose hooks/config then run when a human later runs git in that
// subdir — the justinsteven "buried bare repos + fsmonitor" class,
// github.com/justinsteven/advisories). Pinning the top-level `.git` and
// fail-closing on submodules/linked-worktrees does not enumerate the tree for
// such plants. The residual is bounded (the tree is already writable, so the
// agent can change tracked build scripts regardless — the incremental exposure
// is small) and tracked in #76; the banner tells the human a writable tree is
// not risk-free even with `.git` hardened.
func AssessGitHardening(trees []string) []GitHardeningGap {
	var gaps []GitHardeningGap
	for _, t := range trees {
		if t == "" {
			continue
		}
		gitPath := filepath.Join(t, ".git")
		fi, err := os.Lstat(gitPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue // not a checkout: nothing to harden
			}
			gaps = append(gaps, GitHardeningGap{Tree: t, Reason: fmt.Sprintf(".git could not be inspected (%v); refusing to bind writable without a proven-hardened .git", err)})
			continue
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			gaps = append(gaps, GitHardeningGap{Tree: t, Reason: ".git is a symlink; its target cannot be pinned in place, so the hooks/config surface is not protected"})
		case fi.Mode().IsRegular():
			gaps = append(gaps, GitHardeningGap{Tree: t, Reason: ".git is a file (linked worktree, or a submodule child): no <tree>/.git dir to pin, the .git file is writable, and the exec surfaces live in the shared/parent gitdir"})
		case fi.IsDir():
			// A directory `.git` is pinnable. The residual surface is submodule
			// gitdirs: `.git/modules/*` are separate gitdirs with their own
			// writable hooks/config that top-level pinning does not cover.
			//
			// This is PRESENCE-based: it fires only on a POPULATED `.git/modules`.
			// An UNINITIALIZED submodule superproject (declared in `.gitmodules`
			// but not yet `submodule update --init`, so `.git/modules` is
			// empty/absent) is classified hardenable here — the agent could then
			// create `.git/modules/<name>/hooks/*` that fire on a LATER host
			// `git submodule` op. Deliberately NOT failed-closed: doing so would
			// route every `.gitmodules`-bearing repo to the ephemeral fallback (the
			// #77 interactive-claude trust-prompt cost) to defend a vector that
			// needs a specific later host action, not a routine build. Tracked as a
			// known sub-case in issue #76 instead.
			if hasSubmoduleGitdirs(gitPath) {
				gaps = append(gaps, GitHardeningGap{Tree: t, Reason: "has submodule gitdirs (.git/modules/*) whose hooks/config stay writable and are not yet pinned"})
			}
		default:
			gaps = append(gaps, GitHardeningGap{Tree: t, Reason: fmt.Sprintf(".git is neither a directory nor a regular file (mode %v); refusing to bind writable", fi.Mode())})
		}
	}
	return gaps
}

// hasSubmoduleGitdirs reports whether gitDir contains a non-empty `modules`
// subtree (the submodule gitdir store). It only needs to know "any entry",
// not enumerate them.
func hasSubmoduleGitdirs(gitDir string) bool {
	modules := filepath.Join(gitDir, "modules")
	f, err := os.Open(modules)
	if err != nil {
		return false // no modules dir => no submodule gitdirs (or unreadable => not our concern here)
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	return err == nil && len(names) > 0
}
