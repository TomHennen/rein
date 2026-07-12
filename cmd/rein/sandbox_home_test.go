package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSandboxAllowReadPathsCuratedSet asserts the #59 curated allow-back set:
// the agent config (~/.claude, ~/.claude.json, CLAUDE_CONFIG_DIR) and the
// justified read-mostly toolchain trees are allowed back — and NOTHING broad
// like ~/.local or ~/.config sneaks in (each entry must stay individually
// justified; ~/.local/share holds keyrings, browser profiles, …).
func TestSandboxAllowReadPathsCuratedSet(t *testing.T) {
	home := "/home/someone"
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/someone/dotfiles/claude")
	t.Setenv("PATH", "/usr/bin:/bin") // no under-home agent: install chain contributes nothing

	got := sandboxAllowReadPaths(home, "", []string{"definitely-not-a-real-command-xyz"})
	set := map[string]bool{}
	for _, p := range got {
		set[p] = true
	}
	for _, want := range []string{
		"/home/someone/.claude",
		"/home/someone/dotfiles/claude", // CLAUDE_CONFIG_DIR, env-resolved
		"/home/someone/.claude.json",
		"/home/someone/.local/bin",
		"/home/someone/.cargo",
		"/home/someone/.rustup",
		"/home/someone/go",
		"/home/someone/.pyenv",
	} {
		if !set[want] {
			t.Errorf("curated allow-back %q missing; got %v", want, got)
		}
	}
	for _, tooBroad := range []string{
		"/home/someone",
		"/home/someone/.local",
		"/home/someone/.local/share",
		"/home/someone/.config",
	} {
		if set[tooBroad] {
			t.Errorf("over-broad allow-back %q present; the set must stay minimal: %v", tooBroad, got)
		}
	}
}

// TestInstallChainAllowReads exercises the auto-derivation against a real
// on-disk layout mimicking an npm global install under $HOME: a bin-dir
// symlink farm pointing into lib/node_modules. The chain must contribute the
// bin dir AND the node_modules root (an npm package needs its whole
// node_modules), both in symlink-resolved form.
func TestInstallChainAllowReads(t *testing.T) {
	home := t.TempDir()
	// EvalSymlinks the tempdir itself: /tmp may be a symlink on some hosts,
	// and the derivation compares resolved forms.
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}

	// ~/.npm-global/lib/node_modules/@scope/agent/cli.js  (the real entry)
	// ~/.npm-global/bin/agent -> ../lib/node_modules/@scope/agent/cli.js
	pkgDir := filepath.Join(home, ".npm-global", "lib", "node_modules", "@scope", "agent")
	binDir := filepath.Join(home, ".npm-global", "bin")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(pkgDir, "cli.js")
	if err := os.WriteFile(cli, []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cli, filepath.Join(binDir, "agent")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

	got := installChainAllowReads(home, "agent")
	set := map[string]bool{}
	for _, p := range got {
		set[p] = true
	}
	if !set[binDir] {
		t.Errorf("install chain missing the bin dir %q; got %v", binDir, got)
	}
	nm := filepath.Join(home, ".npm-global", "lib", "node_modules")
	if !set[nm] {
		t.Errorf("install chain missing the node_modules root %q; got %v", nm, got)
	}

	// A native-installer layout (no node_modules): the resolved target's
	// containing dir is contributed instead.
	verDir := filepath.Join(home, ".local", "share", "agent2", "versions")
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin2 := filepath.Join(verDir, "2.0.0")
	if err := os.WriteFile(bin2, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	lb := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(lb, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin2, filepath.Join(lb, "agent2")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", lb+string(os.PathListSeparator)+"/usr/bin:/bin")
	got = installChainAllowReads(home, "agent2")
	set = map[string]bool{}
	for _, p := range got {
		set[p] = true
	}
	if !set[lb] || !set[verDir] {
		t.Errorf("native-install chain = %v, want both %q and %q", got, lb, verDir)
	}

	// A system binary (outside $HOME) contributes NOTHING — the home deny
	// doesn't affect it, and silent widening is forbidden.
	t.Setenv("PATH", "/usr/bin:/bin")
	if got := installChainAllowReads(home, "sh"); len(got) != 0 {
		t.Errorf("system binary contributed allow-backs under home: %v", got)
	}

	// An unresolvable command contributes nothing (launch fails loudly on its
	// own; guessing paths open would be a silent widening).
	if got := installChainAllowReads(home, "no-such-cmd-xyz"); len(got) != 0 {
		t.Errorf("unresolvable command contributed allow-backs: %v", got)
	}
}

// TestDeriveHomeDenial covers the four #59 wiring behaviors end to end
// through the pure helper (S1: previously this logic lived inline in
// runSandboxed and had no test — deleting it or inverting the kill-switch
// branch left the suite green).
func TestDeriveHomeDenial(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", "/usr/bin:/bin")

	home := t.TempDir()
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	workTree := filepath.Join(home, "repo")
	if err := os.MkdirAll(workTree, 0o755); err != nil {
		t.Fatal(err)
	}

	// (1) Default-on: the home deny is produced, resolved, with allow-backs.
	homeDeny, allowReads, showHome, err := deriveHomeDenial("", "", home, "", []string{"claude"}, []string{workTree})
	if err != nil {
		t.Fatalf("default-on: %v", err)
	}
	if showHome {
		t.Error("default-on: showHome = true, want false")
	}
	if homeDeny != home {
		t.Errorf("default-on: homeDeny = %q, want %q", homeDeny, home)
	}
	set := map[string]bool{}
	for _, p := range allowReads {
		set[p] = true
	}
	if !set[filepath.Join(home, ".claude")] {
		t.Errorf("default-on: allow-backs missing ~/.claude; got %v", allowReads)
	}

	// (2) Kill switch: NO home deny, NO allow-backs, showHome reported so the
	// caller prints the loud warning.
	homeDeny, allowReads, showHome, err = deriveHomeDenial("1", "/some/abs", home, "", []string{"claude"}, []string{workTree})
	if err != nil {
		t.Fatalf("kill switch: %v", err)
	}
	if !showHome {
		t.Error("kill switch: showHome = false, want true")
	}
	if homeDeny != "" {
		t.Errorf("kill switch: homeDeny = %q, want empty (deny skipped)", homeDeny)
	}
	if len(allowReads) != 0 {
		t.Errorf("kill switch: allow-backs = %v, want none (moot while $HOME visible)", allowReads)
	}

	// (3) Operator extras merge in SYMLINK-RESOLVED (D6) — and entries under
	// the work tree are dropped.
	target := filepath.Join(home, "tools")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "tools-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, allowReads, _, err = deriveHomeDenial("", link+":"+filepath.Join(workTree, "sub"), home, "", nil, []string{workTree})
	if err != nil {
		t.Fatalf("extras: %v", err)
	}
	set = map[string]bool{}
	for _, p := range allowReads {
		set[p] = true
	}
	if !set[target] {
		t.Errorf("extras: resolved symlink target %q missing; got %v", target, allowReads)
	}
	if set[link] {
		t.Errorf("extras: unresolved alias %q emitted; got %v", link, allowReads)
	}
	if set[filepath.Join(workTree, "sub")] {
		t.Errorf("extras: entry under the work tree not dropped; got %v", allowReads)
	}

	// (4) Malformed REIN_SANDBOX_ALLOW_READ fails closed — in BOTH modes,
	// including under the kill switch that makes it moot.
	if _, _, _, err := deriveHomeDenial("", "relative/path", home, "", nil, []string{workTree}); err == nil {
		t.Error("malformed allow-read accepted in default mode")
	}
	if _, _, _, err := deriveHomeDenial("1", "relative/path", home, "", nil, []string{workTree}); err == nil {
		t.Error("malformed allow-read accepted under the kill switch (must fail closed even when moot)")
	}

	// No home dir and no kill switch: fail closed (launching without the deny
	// would silently expose the home tree).
	if _, _, _, err := deriveHomeDenial("", "", "", "", nil, []string{workTree}); err == nil {
		t.Error("empty home accepted without the kill switch")
	}
}

// TestResolveAllowBacks_Ancestry pins the three ancestry cases against the
// writable paths (#63). The middle one was a LAUNCH-BLOCKING bug: srt re-binds
// allow-backs read-only ON TOP of the writable binds, and skips an allowRead
// only when a write path COVERS it — never when the allowRead is an ANCESTOR of
// one. So an allow-back containing the working tree ro-bound over it, bwrap
// could not create the .gitconfig bind target inside, and the sandbox refused to
// launch. `~/go` is an allow-back and `~/go/src/<pkg>` is the classic GOPATH
// checkout, so a GOPATH user could not run at all.
//
// It survived the whole of #59 because every test in this repo runs from a work
// tree OUTSIDE $HOME, where no allow-back can be an ancestor. These use real
// dirs on disk: the punch-out has to enumerate them.
func TestResolveAllowBacks_Ancestry(t *testing.T) {
	root := t.TempDir()
	// A ~/go-shaped tree with the work tree inside it (the GOPATH layout).
	must := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	goDir := must(filepath.Join(root, "go"))
	must(filepath.Join(goDir, "pkg", "mod"))
	must(filepath.Join(goDir, "bin"))
	must(filepath.Join(goDir, "src", "other"))
	workTree := must(filepath.Join(goDir, "src", "demo")) // work tree UNDER the allow-back
	claude := must(filepath.Join(root, ".claude"))        // unrelated allow-back
	vendor := must(filepath.Join(workTree, "vendor"))     // allow-back UNDER the work tree

	got := resolveAllowBacks([]string{goDir, claude, vendor, workTree}, []string{workTree})
	gotSet := map[string]bool{}
	for _, p := range got {
		gotSet[p] = true
	}

	// (1) The work tree itself and anything under it: DROPPED (already writable;
	// emitting them read-only is what broke the launch).
	for _, p := range []string{workTree, vendor} {
		if gotSet[p] {
			t.Errorf("allow-back %q is at/under the work tree but was emitted read-only; "+
				"srt would ro-bind over the writable tree and the launch would abort", p)
		}
	}
	// (2) The ANCESTOR allow-back is punched out, not emitted whole...
	if gotSet[goDir] {
		t.Fatalf("the ancestor allow-back %q was emitted whole — this ro-binds over the "+
			"work tree and aborts the launch (the #63 bug)", goDir)
	}
	// ...and the punch-out must PRESERVE the read-back that justifies the entry:
	// the Go module cache is the entire reason ~/go is allowed back.
	for _, want := range []string{
		filepath.Join(goDir, "pkg"), // contains pkg/mod — the module cache
		filepath.Join(goDir, "bin"),
		filepath.Join(goDir, "src", "other"), // sibling of the work tree, still readable
	} {
		if !gotSet[want] {
			t.Errorf("punch-out dropped %q; the work tree must stay writable WITHOUT losing "+
				"the rest of the allow-back. got: %v", want, got)
		}
	}
	// (3) Unrelated allow-backs pass through untouched.
	if !gotSet[claude] {
		t.Errorf("unrelated allow-back %q was dropped; got: %v", claude, got)
	}
	// The punch-out may only ever expose LESS than the original allow-back: every
	// emitted path must still live under one of the inputs.
	for _, p := range got {
		if !pathAtOrUnder(p, goDir) && !pathAtOrUnder(p, claude) {
			t.Errorf("punch-out emitted %q, which is outside every requested allow-back — "+
				"it must narrow, never widen", p)
		}
	}
}

// TestResolveAllowBacks_MultipleWritables: the work tree is not the only rw bind
// (the agent scratch dir is an ExtraAllowWrite, and lands under $HOME whenever
// TMPDIR does). An allow-back ancestor of ANY writable path aborts the launch.
func TestResolveAllowBacks_MultipleWritables(t *testing.T) {
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	scratch := filepath.Join(tools, "scratch")
	keep := filepath.Join(tools, "keep")
	for _, p := range []string{scratch, keep} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := resolveAllowBacks([]string{tools}, []string{filepath.Join(root, "elsewhere"), scratch})
	for _, p := range got {
		if pathAtOrUnder(scratch, p) {
			t.Errorf("emitted allow-back %q is an ancestor of the writable scratch dir %q — "+
				"srt would ro-bind over it", p, scratch)
		}
	}
	var sawKeep bool
	for _, p := range got {
		if p == keep {
			sawKeep = true
		}
	}
	if !sawKeep {
		t.Errorf("punch-out dropped the unrelated sibling %q; got %v", keep, got)
	}
}

// TestCredentialDenyReadHidesCargoCredentials: the cargo registry token files
// are on the targeted denylist — env-resolved (CARGO_HOME) and default — so
// they stay hidden UNDER the ~/.cargo allow-back (srt's file-deny beats
// dir-allow, verified 0.0.63; Build asserts the entries coexist).
func TestCredentialDenyReadHidesCargoCredentials(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	t.Setenv("XDG_CONFIG_HOME", "/home/someone/.config")
	t.Setenv("CARGO_HOME", "")

	paths, err := credentialDenyReadPaths(t.TempDir())
	if err != nil {
		t.Fatalf("credentialDenyReadPaths: %v", err)
	}
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	for _, want := range []string{
		"/home/someone/.cargo/credentials.toml",
		"/home/someone/.cargo/credentials",
	} {
		if !set[want] {
			t.Errorf("cargo credential file %q missing from deny-read set: %v", want, paths)
		}
	}

	// Relocated CARGO_HOME: relocated files denied AND the defaults stay.
	t.Setenv("CARGO_HOME", "/home/someone/dotfiles/cargo")
	paths, err = credentialDenyReadPaths(t.TempDir())
	if err != nil {
		t.Fatalf("credentialDenyReadPaths: %v", err)
	}
	set = map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	for _, want := range []string{
		"/home/someone/dotfiles/cargo/credentials.toml",
		"/home/someone/.cargo/credentials.toml", // default still hidden
	} {
		if !set[want] {
			t.Errorf("cargo credential file %q missing when CARGO_HOME set: %v", want, paths)
		}
	}
}
