// Wholesale $HOME hiding for sandboxed runs (issue #59, phase1-design §4.2).
//
// The maintained credential denylist (credentialDenyReadPaths) can never win
// the race against unknown-unknown same-uid credential stores — every new
// tool drops another token file somewhere under $HOME. So sandboxed runs
// deny-read the ENTIRE home directory by default and allow back a small,
// enumerable, justified set: the agent's own install chain, its config, and a
// few read-mostly toolchain trees. The targeted denylist stays layered on top
// as belt-and-suspenders (srt applies deeper denies after shallower
// allow-backs, so a credential path inside an allowed-back dir stays hidden —
// verified against srt 0.0.63 linux-sandbox-utils.js).
//
// Failure mode by design: an unlisted tool breaks LOUDLY (its $HOME path
// reads as absent/empty) instead of an unlisted credential store leaking
// silently. The run banner prints the exact remediation
// (REIN_SANDBOX_ALLOW_READ=... or the REIN_SANDBOX_SHOW_HOME=1 kill switch)
// so the discovery loop is self-serve. Deliberately NO interactive "allow
// this dir?" prompt — rubber-stamping risk defeats the model (#59).
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/srt"
)

// deriveHomeDenial is the PURE decision core of the #59 home-denial wiring:
// given the two escape-hatch env values, the (possibly unresolved) home dir,
// the resolved srt path, the agent argv, and the resolved working tree, it
// returns the home deny path (empty when the kill switch is on), the merged
// allow-back set, whether the kill switch is engaged (the caller prints the
// loud warning), and a fail-closed error. Extracted from runSandboxed so
// every behavior is unit-testable — deleting the wiring or inverting the
// kill-switch branch must fail the suite, not just a live run.
//
// Fail-closed rules, all tested:
//   - the REIN_SANDBOX_ALLOW_READ value is parsed (and can fail the launch)
//     even when REIN_SANDBOX_SHOW_HOME makes it moot — otherwise the operator
//     learns a broken syntax "works" and hits the error only after dropping
//     the kill switch;
//   - no home dir and no kill switch => error (launching without the deny
//     would silently expose the home tree);
//   - operator extras are symlink-resolved here (Build re-resolves as the
//     enforcement backstop) so dedupe, the work-tree filter, and the banner
//     all see the real paths.
//
// writables is every path srt binds READ-WRITE (the working tree first, then
// each ExtraAllowWrite — e.g. the agent scratch dir, which lands under $HOME
// whenever TMPDIR does). resolveAllowBacks reconciles the allow-back set
// against ALL of them: any one of them being ro-bound by an allow-back ancestor
// aborts the launch (#63).
func deriveHomeDenial(showHomeEnv, allowReadEnv, home, srtPath string, cmdline []string, writables []string) (homeDeny string, allowReads []string, showHome bool, err error) {
	showHome = srt.ShowHomeFromEnv(showHomeEnv)
	extras, err := srt.ParseSandboxAllowRead(allowReadEnv)
	if err != nil {
		return "", nil, showHome, fmt.Errorf("parse %s: %w", srt.EnvSandboxAllowRead, err)
	}
	if showHome {
		// Kill switch: no deny, no allow-backs (moot while $HOME is visible).
		return "", nil, true, nil
	}
	if home == "" {
		return "", nil, false, fmt.Errorf("cannot resolve home dir to hide it in-sandbox; set $HOME, or explicitly opt out with %s=1", srt.EnvSandboxShowHome)
	}
	if homeDeny, err = proxy.ResolveAbs(home); err != nil {
		return "", nil, false, fmt.Errorf("resolve home dir symlinks: %w", err)
	}
	allowReads = sandboxAllowReadPaths(homeDeny, srtPath, cmdline)
	for _, e := range extras {
		r, rerr := proxy.ResolveAbs(e)
		if rerr != nil {
			return "", nil, false, fmt.Errorf("resolve %s entry %q: %w", srt.EnvSandboxAllowRead, e, rerr)
		}
		allowReads = append(allowReads, r)
	}
	// Reconcile against every read-WRITE bind: drop allow-backs that are already
	// writable, and punch the writable paths out of any allow-back that contains
	// them. Without this, an allow-back ancestor of the working tree ro-binds over
	// it and srt cannot launch at all (#63).
	allowReads = resolveAllowBacks(dedupePaths(allowReads), writables)
	return homeDeny, allowReads, false, nil
}

// sandboxAllowReadPaths returns the auto-derived read-only allow-back set for
// a run whose $HOME is deny-read wholesale. home must be the symlink-resolved
// home directory. srtPath is the resolved srt binary (from preflight);
// cmdline is the agent argv (after "--") — both install chains are derived so
// the flip cannot brick the very binaries the run depends on. Every entry is
// justified inline; keep this list MINIMAL — each addition widens what a
// (possibly prompt-injected) agent can read, and the REIN_SANDBOX_ALLOW_READ
// escape hatch makes user-specific additions self-serve. Entries for absent
// paths are harmless (srt skips them).
func sandboxAllowReadPaths(home, srtPath string, cmdline []string) []string {
	var out []string

	// (1) The agent's own install chain. Claude Code and friends commonly
	// live UNDER $HOME (~/.npm-global, ~/.nvm, ~/.local/share/claude); hiding
	// $HOME without allowing the install back means the launched command
	// simply vanishes in-sandbox. Derived, not hardcoded, so any agent works.
	if len(cmdline) > 0 {
		out = append(out, installChainAllowReads(home, cmdline[0])...)
	}
	// srt's OWN install chain. srt executes pieces of its npm package INSIDE
	// the bwrap namespace — observed live (E2E on this box): the in-sandbox
	// bootstrap runs .../sandbox-runtime/vendor/seccomp/<arch>/apply-seccomp,
	// so an npm-global srt under $HOME dies with exit 127 ("No such file or
	// directory") under the home tmpfs before the child ever starts. That
	// failure is CLOSED (the pre-launch self-test catches it) but would brick
	// every run on such a box without this allow-back.
	if srtPath != "" {
		out = append(out, installChainAllowReads(home, srtPath)...)
	}
	// The node runtime itself: npm-installed agents are `#!/usr/bin/env node`
	// scripts, so the interpreter must be readable too. When node is
	// system-wide (/usr/bin/node) this contributes nothing; when it is under
	// $HOME (nvm, ~/.npm-global) it contributes node's own install prefix.
	out = append(out, installChainAllowReads(home, "node")...)

	// (2) The wrapped agent's config + credentials are DELIBERATELY NOT allowed
	// back (issue #94, default-deny flip). The host's ~/.claude and ~/.claude.json
	// are fully denied in-sandbox (credentialDenyReadPaths); claude is repointed
	// at a rein-owned persistent overlay via CLAUDE_CONFIG_DIR (sandbox_claude_home.go,
	// internal/srt/env.go), which is bound read-WRITE via ExtraAllowWrite and holds
	// the seeded .credentials.json. So the agent authenticates from the overlay, never
	// the host tree — no allow-back into ~/.claude is needed, and the developer's
	// cross-project history can no longer leak by default.

	// (3) Curated read-mostly toolchain trees. Criteria for inclusion:
	// binaries/caches a build or the agent's subprocesses EXECUTE or READ,
	// no known credential files (or the credential file is explicitly
	// file-denied in credentialDenyReadPaths and srt's exact-match rule keeps
	// it denied under the dir allow-back — the ~/.cargo case). Actively
	// WRITTEN caches (~/.npm, ~/.cache) are deliberately NOT here: an
	// allow-back is a READ-ONLY bind, which would turn their writes into
	// EROFS failures, whereas under the deny tmpfs they are simply empty and
	// writable (cold cache, ephemeral — degraded but working).
	out = append(out,
		// user-level executables on PATH (pipx installs, npm bin links, the
		// claude native-installer launcher). Executables, not data.
		filepath.Join(home, ".local", "bin"),
		// rustup-managed Rust: ~/.cargo holds the cargo/rustc shims and the
		// registry cache, ~/.rustup the toolchains themselves; a rustup setup
		// cannot compile anything without both. ~/.cargo/credentials(.toml)
		// is explicitly file-denied and stays hidden under this allow-back.
		filepath.Join(home, ".cargo"),
		filepath.Join(home, ".rustup"),
		// GOPATH: the Go module cache (~/go/pkg/mod) — with registry egress
		// denied by default, a hidden module cache turns every Go build into
		// a failed download. Content-addressed source code, no credentials.
		filepath.Join(home, "go"),
		// pyenv-managed Python interpreters: read-mostly binary trees; a
		// pyenv-selected python otherwise vanishes mid-run. No credentials.
		filepath.Join(home, ".pyenv"),
	)

	return dedupePaths(out)
}

// agentUnderClaudeDenyError detects the `claude migrate-installer` layout, where the
// agent binary resolves UNDER ~/.claude (e.g. ~/.claude/local/claude, with a launcher
// symlinked from ~/.local/bin). Since #94 default-denies ~/.claude in-sandbox, that
// install tree is hidden and the auto-derived install-chain allow-back would sit under
// the authoritative ~/.claude deny — srt.Build then aborts with a cryptic
// widening-under-deny error whose only workaround is the insecure SHOW_HOME kill switch.
// Detect it early and return a CLEAR, actionable error instead. home must be the
// symlink-resolved home dir; returns nil when the layout is fine or undetectable (a
// launch that would fail for another reason still fails on its own).
//
// Only meaningful while the home deny is active — the caller gates on that (under
// SHOW_HOME, ~/.claude is visible and the layout launches fine).
func agentUnderClaudeDenyError(home string, cmdline []string) error {
	if home == "" || len(cmdline) == 0 {
		return nil
	}
	claudeDir, err := proxy.ResolveAbs(filepath.Join(home, ".claude"))
	if err != nil {
		return nil
	}
	found, err := exec.LookPath(cmdline[0])
	if err != nil {
		return nil
	}
	// Check the launcher AND its symlink target: the native launcher can live at
	// ~/.local/bin/claude (outside ~/.claude) yet point INTO ~/.claude/local.
	candidates := []string{found}
	if t, terr := filepath.EvalSymlinks(found); terr == nil {
		candidates = append(candidates, t)
	}
	for _, c := range candidates {
		r, rerr := proxy.ResolveAbs(c)
		if rerr != nil {
			continue
		}
		if pathAtOrUnder(r, claudeDir) {
			return fmt.Errorf("the %q binary resolves under ~/.claude (%s) — the `claude migrate-installer` layout. "+
				"rein hides ~/.claude in-sandbox (issue #94 default-deny), so that install tree is invisible and the "+
				"sandbox cannot launch it. Reinstall claude with the NATIVE installer so it lives under ~/.local "+
				"(e.g. `curl -fsSL https://claude.ai/install.sh | bash`), then re-run. "+
				"(Proper support for the ~/.claude/local layout is tracked in issue #132.)",
				cmdline[0], r)
		}
	}
	return nil
}

// installChainAllowReads resolves command via PATH and returns the allow-back
// prefixes needed to execute it in-sandbox, filtered to paths under home
// (paths outside $HOME are unaffected by the home deny). Two entries at most:
//
//   - the directory of the PATH entry itself — typically a symlink farm
//     (~/.npm-global/bin, ~/.local/bin) whose links must be readable;
//   - for the symlink-RESOLVED target: its npm tree root when it lives under
//     a node_modules (an npm package needs its whole node_modules, not just
//     its own dir), otherwise its containing directory (e.g. the claude
//     native installer's ~/.local/share/claude/versions).
//
// A command that cannot be resolved contributes nothing — the launch will
// fail loudly on its own, and guessing paths open would be a silent widening.
//
// Known rare gap (deliberate): a PATH bin dir under $HOME that is ITSELF a
// symlink to OUTSIDE $HOME (e.g. ~/bin -> /opt/tools/bin). appendIfUnderHome
// filters on the RESOLVED form, which is outside home, so nothing is
// contributed — but the in-sandbox PATH still says ~/bin, the alias path is
// hidden by the home tmpfs, and the command vanishes. REIN_SANDBOX_ALLOW_READ
// of the outside-home target is a no-op for the alias (srt binds the real
// path, not the symlink), so only REIN_SANDBOX_SHOW_HOME=1 recovers. Accepted:
// the layout is rare, the failure is loud, and auto-allowing the symlink path
// itself would re-expose a home-resident alias file chosen by the agent's
// environment.
func installChainAllowReads(home, command string) []string {
	found, err := exec.LookPath(command)
	if err != nil {
		return nil
	}
	abs, err := filepath.Abs(found)
	if err != nil {
		return nil
	}
	var out []string
	out = appendIfUnderHome(out, home, filepath.Dir(abs))
	if target, err := filepath.EvalSymlinks(abs); err == nil {
		if i := strings.LastIndex(target, "/node_modules/"); i >= 0 {
			out = appendIfUnderHome(out, home, target[:i+len("/node_modules")])
		} else {
			out = appendIfUnderHome(out, home, filepath.Dir(target))
		}
	}
	return out
}

// appendIfUnderHome appends p (symlink-resolved) when it sits STRICTLY under
// home. Equal-to-home is excluded: allowing $HOME back wholesale would undo
// the deny (Build rejects it; the LOUD kill switch is the sanctioned route).
func appendIfUnderHome(out []string, home, p string) []string {
	r, err := proxy.ResolveAbs(p)
	if err != nil {
		return out
	}
	rel, err := filepath.Rel(home, r)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return out
	}
	return append(out, r)
}

// resolveAllowBacks reconciles the read-only allow-back set against the paths
// that MUST stay writable (the working tree + every ExtraAllowWrite). All
// inputs must be absolute and symlink-resolved.
//
// THE INVARIANT: the working tree is writable in-sandbox, always. No allow-back
// may take that away. Everything below exists to keep it true.
//
// Why this is not merely cosmetic (the #63 launch-blocking bug): srt re-binds
// allow-backs READ-ONLY on top of the $HOME deny tmpfs, and it emits those
// ro-binds AFTER the writable binds (pushReadDenyDirMounts, linux-sandbox-utils.js).
// It skips an allowRead only when a write path COVERS it — never when the
// allowRead is an ANCESTOR of a write path. So an allow-back that contains the
// working tree ro-binds straight over it and the tree goes read-only. bwrap then
// cannot even create the .gitconfig bind target inside it and the launch ABORTS:
//
//	bwrap: Can't create file at .../.gitconfig: Read-only file system
//
// That is not exotic — `~/go` is an allow-back (the module cache) and
// `~/go/src/<pkg>` is the classic GOPATH checkout, so a GOPATH user could not
// launch at all. It failed CLOSED (the pre-launch self-test caught it), but it
// bricked the run. It went unnoticed because every test in this repo runs from a
// work tree OUTSIDE $HOME, where no allow-back can possibly be an ancestor.
//
// Three ancestry cases, each pinned in sandbox_home_test.go:
//
//   - allow-back AT or UNDER a writable path -> DROP it. The path is already
//     bound read-write; an allowRead would be redundant, and listing it in the
//     banner as "read-only" would be a lie.
//
//   - allow-back is a strict ANCESTOR of a writable path -> PUNCH IT OUT: descend,
//     keeping every child that does NOT contain a writable path and recursing into
//     the one that does. `~/go` with a work tree at `~/go/src/demo` becomes
//     `~/go/bin`, `~/go/pkg`, `~/go/src/<everything-but-demo>` — so the Go module
//     cache (~/go/pkg/mod, the whole reason ~/go is allowed back) stays readable
//     while the checkout stays WRITABLE. Strictly narrower than the original
//     allow-back: punching out can only ever expose LESS, never more.
//
//   - allow-back unrelated to any writable path -> keep as-is.
//
// If a directory we must descend into cannot be read, the allow-back is DROPPED
// rather than emitted whole: a lost read-back degrades the run (a tool may need
// a narrow REIN_SANDBOX_ALLOW_READ), whereas emitting the ancestor would brick it.
//
// This applies uniformly to the auto-derived allow-backs AND to operator-supplied
// REIN_SANDBOX_ALLOW_READ entries — an operator who allow-reads an ancestor of
// their own work tree would otherwise brick their run in exactly the same way.
func resolveAllowBacks(paths, writables []string) []string {
	var out []string
	for _, p := range paths {
		out = append(out, punchOutWritables(p, writables)...)
	}
	return dedupePaths(out)
}

// punchOutWritables returns the read-only allow-back entries to emit for p given
// the writable paths, per the three ancestry cases in resolveAllowBacks.
func punchOutWritables(p string, writables []string) []string {
	for _, w := range writables {
		if pathAtOrUnder(p, w) {
			return nil // already writable — never re-bind it read-only
		}
	}
	// Which writables sit strictly inside p? Those are the holes to punch.
	var inside []string
	for _, w := range writables {
		if pathAtOrUnder(w, p) {
			inside = append(inside, w)
		}
	}
	if len(inside) == 0 {
		return []string{p} // unrelated to every writable — emit whole
	}
	// p contains a writable path. Emitting p would ro-bind over it. Descend.
	entries, err := os.ReadDir(p)
	if err != nil {
		// Cannot enumerate => cannot punch out safely => drop. Dropping degrades;
		// emitting would make the working tree read-only and abort the launch.
		return nil
	}
	var out []string
	for _, e := range entries {
		child := filepath.Join(p, e.Name())
		// A non-directory can never contain a writable path — emit it directly
		// (and never ReadDir it).
		if !e.IsDir() {
			out = append(out, child)
			continue
		}
		out = append(out, punchOutWritables(child, writables)...)
	}
	return out
}

// pathAtOrUnder reports whether p is AT or UNDER base (p == base, or p nested
// inside it). Both must be absolute and cleaned. Compares path COMPONENTS, not
// string prefixes, so /home/dev-old is correctly NOT "under" /home/dev.
func pathAtOrUnder(p, base string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func dedupePaths(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
