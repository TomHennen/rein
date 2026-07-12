package worktree

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGit builds a GitOf that answers from a map (dir -> origin URL), treating
// each mapped dir as a healthy checkout ROOT. A dir not in the map errors,
// standing in for "not a git checkout / no origin".
func fakeGit(m map[string]string) func(string) (GitInfo, error) {
	return func(dir string) (GitInfo, error) {
		if u, ok := m[dir]; ok {
			return GitInfo{Origin: u, Root: dir}, nil
		}
		return GitInfo{}, os.ErrNotExist
	}
}

// fakeGitInfo answers with fully-specified GitInfo values (for the subdir and
// linked-worktree cases, where Root != dir or Linked is set).
func fakeGitInfo(m map[string]GitInfo) func(string) (GitInfo, error) {
	return func(dir string) (GitInfo, error) {
		if i, ok := m[dir]; ok {
			return i, nil
		}
		return GitInfo{}, os.ErrNotExist
	}
}

// mkCheckout makes dir look like a git checkout (a .git entry is all
// isGitCheckout requires; the remote comes from the injected RemoteOf).
func mkCheckout(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRepoFromRemoteURL(t *testing.T) {
	for _, tc := range []struct {
		url, want string
	}{
		{"https://github.com/owner/name.git", "owner/name"},
		{"https://github.com/owner/name", "owner/name"},
		{"https://x-access-token:ghs_secret@github.com/owner/name.git", "owner/name"},
		{"http://github.com/owner/name.git", "owner/name"},
		{"ssh://git@github.com/owner/name.git", "owner/name"},
		{"ssh://git@github.com:22/owner/name.git", "owner/name"},
		{"git@github.com:owner/name.git", "owner/name"},
		{"git@github.com:owner/name", "owner/name"},
		{"GIT@GITHUB.COM:Owner/Name.git", "Owner/Name"},
	} {
		got, err := RepoFromRemoteURL(tc.url)
		if err != nil || got != tc.want {
			t.Errorf("RepoFromRemoteURL(%q) = %q, %v; want %q", tc.url, got, err, tc.want)
		}
	}

	// FAIL CLOSED on everything that is not a github.com owner/repo. The
	// non-github host cases are the load-bearing ones: a remote at
	// evil.example.com/owner/name must never satisfy "is this a checkout of
	// owner/name?", or a mapped path could smuggle an unrelated tree past
	// validation under a session repo's name.
	for _, bad := range []string{
		"",
		"https://gitlab.com/owner/name.git",
		"https://github.com.evil.example/owner/name.git",
		"git@evil.example.com:owner/name.git",
		"https://github.com/owner",
		"/srv/git/mirror.git",
		"../relative/path",
		"file:///srv/git/name.git",
	} {
		if got, err := RepoFromRemoteURL(bad); err == nil {
			t.Errorf("RepoFromRemoteURL(%q) = %q, want an error (fail closed)", bad, got)
		}
	}
}

func TestParseEnv(t *testing.T) {
	got, err := ParseEnv("owner/a=/srv/a:owner/b=/srv/b:")
	if err != nil {
		t.Fatalf("ParseEnv: %v", err)
	}
	if got["owner/a"] != "/srv/a" || got["owner/b"] != "/srv/b" || len(got) != 2 {
		t.Fatalf("ParseEnv = %v", got)
	}
	if m, err := ParseEnv("  "); err != nil || m != nil {
		t.Errorf("empty value must parse to nil, got %v, %v", m, err)
	}
	for _, bad := range []string{
		"owner/a",               // no '='
		"owner/a=relative",      // not absolute
		"owner/a=~/dev/a",       // tilde is not expanded — fail closed
		"nota-repo=/srv/a",      // key not owner/repo
		"owner/a=/x:owner/a=/y", // same repo twice
	} {
		if _, err := ParseEnv(bad); err == nil {
			t.Errorf("ParseEnv(%q) must fail closed", bad)
		}
	}
}

// TestResolveBindsMappedCheckout is the happy path: a session repo whose local
// checkout is named in the `worktrees:` map becomes a writable binding, and the
// working tree's own repo is autodetected from its remote.
func TestResolveBindsMappedCheckout(t *testing.T) {
	root := t.TempDir()
	work := mkCheckout(t, filepath.Join(root, "a"))
	b := mkCheckout(t, filepath.Join(root, "b"))

	res, err := Resolve(Params{
		SessionRepos: []string{"owner/a", "owner/b"},
		FileMap:      map[string]string{"owner/b": b},
		WorkTree:     work,
		Home:         filepath.Join(root, "home"),
		GitOf: fakeGit(map[string]string{
			work: "https://github.com/owner/a.git",
			b:    "git@github.com:owner/b.git",
		}),
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.WorkTreeRepo != "owner/a" {
		t.Errorf("working-tree repo autodetect = %q, want owner/a", res.WorkTreeRepo)
	}
	if len(res.Bindings) != 1 || res.Bindings[0].Repo != "owner/b" || res.Bindings[0].Path != b || res.Bindings[0].Mode != "rw" {
		t.Fatalf("bindings = %+v", res.Bindings)
	}
	if res.Bindings[0].Source != "session" {
		t.Errorf("source = %q, want session", res.Bindings[0].Source)
	}

	// The agent-visible payload must name BOTH trees (it cannot guess either),
	// with the in-sandbox == host paths.
	var got []Binding
	if err := json.Unmarshal([]byte(AgentEnvValue(res.AgentBindings(work))), &got); err != nil {
		t.Fatalf("agent env JSON: %v", err)
	}
	if len(got) != 2 || got[0].Repo != "owner/a" || got[0].Path != work || got[1].Repo != "owner/b" || got[1].Path != b {
		t.Fatalf("agent bindings = %+v", got)
	}
	for _, g := range got {
		if g.Mode != "rw" {
			t.Errorf("agent binding %s mode = %q, want rw", g.Repo, g.Mode)
		}
	}
}

// TestResolveEnvOverridesSession: REIN_WORKTREES wins over the session file for
// the same repo (the per-run override), and its provenance is reported.
func TestResolveEnvOverridesSession(t *testing.T) {
	root := t.TempDir()
	work := mkCheckout(t, filepath.Join(root, "a"))
	stale := mkCheckout(t, filepath.Join(root, "b-old"))
	fresh := mkCheckout(t, filepath.Join(root, "b-new"))

	res, err := Resolve(Params{
		SessionRepos: []string{"owner/a", "owner/b"},
		FileMap:      map[string]string{"owner/b": stale},
		EnvValue:     "owner/b=" + fresh,
		WorkTree:     work,
		Home:         filepath.Join(root, "home"),
		GitOf: fakeGit(map[string]string{
			work:  "https://github.com/owner/a",
			stale: "https://github.com/owner/b",
			fresh: "https://github.com/owner/b",
		}),
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Bindings) != 1 || res.Bindings[0].Path != fresh || res.Bindings[0].Source != "env" {
		t.Fatalf("env override did not win: %+v", res.Bindings)
	}
}

// TestResolveFailsClosed covers every refusal. Each of these, if it were a
// silent skip or a silent bind, is a real hole: binding the WRONG tree writable
// hands a (possibly prompt-injected) agent an unrelated project; skipping a
// named tree silently sends it to clone a stale copy instead.
func TestResolveFailsClosed(t *testing.T) {
	root := t.TempDir()
	work := mkCheckout(t, filepath.Join(root, "a"))
	b := mkCheckout(t, filepath.Join(root, "b"))
	other := mkCheckout(t, filepath.Join(root, "other"))
	plain := filepath.Join(root, "plaindir") // exists, but NOT a checkout
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	remotes := fakeGit(map[string]string{
		work:  "https://github.com/owner/a.git",
		b:     "https://github.com/owner/b.git",
		other: "https://github.com/owner/somethingelse.git",
		home:  "https://github.com/owner/b.git", // pretend even this "is" the repo
	})
	base := func(fileMap map[string]string, env string) Params {
		return Params{
			SessionRepos: []string{"owner/a", "owner/b"},
			FileMap:      fileMap,
			EnvValue:     env,
			WorkTree:     work,
			Home:         home,
			GitOf:        remotes,
		}
	}

	for name, p := range map[string]Params{
		// The mapped path is a checkout of a DIFFERENT repo than the key claims.
		"remote mismatch": base(map[string]string{"owner/b": other}, ""),
		// The mapped path exists but is not a git checkout at all.
		"not a checkout": base(map[string]string{"owner/b": plain}, ""),
		// The mapped path does not exist (srt would silently drop the bind).
		"missing path": base(map[string]string{"owner/b": filepath.Join(root, "nope")}, ""),
		// The repo is not in the session's ceiling: rein would never mint a
		// credential for it, so a writable tree for it is incoherent.
		"repo out of scope": base(map[string]string{"owner/z": b}, ""),
		// $HOME (or an ancestor) as the "checkout" would bind the whole home
		// tree writable and blow a hole through the #59 model.
		"home as worktree": base(map[string]string{"owner/b": home}, ""),
		// A parent of the working tree widens far past one repo.
		"contains work tree": base(map[string]string{"owner/b": root}, ""),
		// Relative path via the env override.
		"relative env path": base(nil, "owner/b=dev/b"),
		// The mapped path IS the working tree but is a checkout of a DIFFERENT
		// repo. The "already writable, skip it" shortcut must not swallow this:
		// the human believes repo B is mapped, and it is not. (Caught live in
		// the #64 demo — the redundancy skip used to run before the identity
		// check.)
		"mismatch at the working tree": base(map[string]string{"owner/b": work}, ""),
	} {
		if _, err := Resolve(p); err == nil {
			t.Errorf("%s: Resolve succeeded, want a fail-closed error", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}

	// A relative path in the SESSION map is caught too.
	if _, err := Resolve(base(map[string]string{"owner/b": "relative/b"}, "")); err == nil {
		t.Error("relative session path: Resolve succeeded, want an error")
	}
}

// TestResolveRejectsNestedBindings: two mapped checkouts must not overlap —
// srt.Build only checks widening-vs-deny, never widening-vs-widening, so this
// check is ours.
func TestResolveRejectsNestedBindings(t *testing.T) {
	root := t.TempDir()
	work := mkCheckout(t, filepath.Join(root, "w"))
	outer := mkCheckout(t, filepath.Join(root, "outer"))
	inner := mkCheckout(t, filepath.Join(root, "outer", "inner"))

	_, err := Resolve(Params{
		SessionRepos: []string{"owner/w", "owner/outer", "owner/inner"},
		FileMap:      map[string]string{"owner/outer": outer, "owner/inner": inner},
		WorkTree:     work,
		Home:         filepath.Join(root, "home"),
		GitOf: fakeGit(map[string]string{
			work:  "https://github.com/owner/w",
			outer: "https://github.com/owner/outer",
			inner: "https://github.com/owner/inner",
		}),
	})
	if err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("nested bindings must be rejected; got %v", err)
	}
}

// TestResolveInsideWorkTreeIsRedundant: a mapped path that lives INSIDE the
// working tree is already writable. Not an error, but never silent.
func TestResolveInsideWorkTreeIsRedundant(t *testing.T) {
	root := t.TempDir()
	work := mkCheckout(t, filepath.Join(root, "a"))
	nested := mkCheckout(t, filepath.Join(work, "vendor", "b"))

	res, err := Resolve(Params{
		SessionRepos: []string{"owner/a", "owner/b"},
		FileMap:      map[string]string{"owner/b": nested},
		WorkTree:     work,
		Home:         filepath.Join(root, "home"),
		GitOf: fakeGit(map[string]string{
			work:   "https://github.com/owner/a",
			nested: "https://github.com/owner/b",
		}),
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Bindings) != 0 {
		t.Errorf("a path inside the working tree must not be re-bound: %+v", res.Bindings)
	}
	if len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, " "), "already writable") {
		t.Errorf("the redundant entry must be reported, not silently dropped: %v", res.Warnings)
	}
}

// TestResolveWorkTreeWarnsButDoesNotBlock: the working tree is bound writable
// today regardless of its remote (pre-#64 behavior, and `rein run` outside a
// checkout is legal). An undetectable or out-of-scope working-tree repo must
// therefore WARN, not refuse — a hard fail here would regress existing runs.
func TestResolveWorkTreeWarnsButDoesNotBlock(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "notarepo") // no .git, no remote

	res, err := Resolve(Params{
		SessionRepos: []string{"owner/a"},
		WorkTree:     work,
		GitOf:        fakeGit(nil),
	})
	if err != nil {
		t.Fatalf("an undetectable working tree must not fail the launch: %v", err)
	}
	if res.WorkTreeRepo != "" || len(res.Warnings) == 0 {
		t.Errorf("want no detected repo + a warning; got %q / %v", res.WorkTreeRepo, res.Warnings)
	}
	if v := AgentEnvValue(res.AgentBindings(work)); v != "" {
		t.Errorf("with nothing detected or mapped, the agent env value must be empty; got %q", v)
	}

	// Out-of-scope working tree: also a warning, also not fatal.
	wc := mkCheckout(t, filepath.Join(root, "c"))
	res, err = Resolve(Params{
		SessionRepos: []string{"owner/a"},
		WorkTree:     wc,
		GitOf:        fakeGit(map[string]string{wc: "https://github.com/owner/c"}),
	})
	if err != nil {
		t.Fatalf("out-of-scope working tree must not fail the launch: %v", err)
	}
	if res.WorkTreeRepo != "" {
		t.Errorf("an out-of-scope working tree must not be reported as the session repo: %q", res.WorkTreeRepo)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "NOT in this session's scope") {
		t.Errorf("want an out-of-scope warning; got %v", res.Warnings)
	}
}

// TestGitRemoteOriginReadsRealRepo exercises the REAL git path (no fake), so a
// change to the git invocation is caught: a symlinked path, a linked worktree,
// and the plain clone case all have to yield the owner/repo.
func TestGitRemoteOriginReadsRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "r")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://github.com/owner/r.git"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	info, err := GitInspect(repo)
	if err != nil {
		t.Fatalf("GitInspect: %v", err)
	}
	if got, err := RepoFromRemoteURL(info.Origin); err != nil || got != "owner/r" {
		t.Fatalf("RepoFromRemoteURL(%q) = %q, %v", info.Origin, got, err)
	}
	if info.Linked {
		t.Error("a plain clone must not be reported as a linked worktree")
	}

	// And end to end through Resolve, with the real remote reader.
	work := mkCheckout(t, filepath.Join(dir, "w"))
	res, err := Resolve(Params{
		SessionRepos: []string{"owner/w", "owner/r"},
		FileMap:      map[string]string{"owner/r": repo},
		WorkTree:     work,
		Home:         filepath.Join(dir, "home"),
	})
	if err != nil {
		t.Fatalf("Resolve with the real git reader: %v", err)
	}
	if len(res.Bindings) != 1 || res.Bindings[0].Repo != "owner/r" {
		t.Fatalf("bindings = %+v", res.Bindings)
	}
	// `work` has a .git dir but no remote at all -> warning, no detected repo.
	if res.WorkTreeRepo != "" {
		t.Errorf("a checkout with no origin must not resolve to a repo: %q", res.WorkTreeRepo)
	}
}

// TestGitRemoteOriginNoOrigin: a checkout with no `origin` remote is an error
// (the caller turns that into a refusal for a mapped path).
func TestGitRemoteOriginNoOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	cmd := exec.Command("git", "-C", repo, "init", "-q")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if _, err := GitInspect(repo); err == nil {
		t.Error("a checkout with no origin remote must error")
	}
}

// TestResolveRejectsBrokenCheckouts pins the two shapes that VALIDATE as
// checkouts but cannot actually work inside the sandbox — a fail-OPEN into a
// broken run, which is worse than a refusal because the banner tells the human
// (and REIN_REPO_WORKTREES tells the agent) that repo B is ready to work in:
//
//   - a LINKED git worktree, whose metadata lives in the MAIN repo's
//     .git/worktrees/<name>, outside the bind (and under the #59 home deny,
//     inside a tmpfs) — every git command in it dies "not a git repository";
//   - a SUBDIRECTORY of a checkout: `git -C` walks UP, so it answers the remote
//     query happily, but only the subdir is bound and its .git is outside it.
//
// Both were found by the #64 reviewer and reproduced live against srt 0.0.63.
func TestResolveRejectsBrokenCheckouts(t *testing.T) {
	root := t.TempDir()
	work := mkCheckout(t, filepath.Join(root, "a"))
	linked := mkCheckout(t, filepath.Join(root, "linked"))
	sub := mkCheckout(t, filepath.Join(root, "b", "sub"))
	realRoot := filepath.Join(root, "b")

	git := fakeGitInfo(map[string]GitInfo{
		work:   {Origin: "https://github.com/owner/a", Root: work},
		linked: {Origin: "https://github.com/owner/b", Root: linked, Linked: true},
		sub:    {Origin: "https://github.com/owner/b", Root: realRoot}, // git walked UP
	})
	for name, path := range map[string]string{
		"linked worktree":        linked,
		"subdirectory of a repo": sub,
	} {
		_, err := Resolve(Params{
			SessionRepos: []string{"owner/a", "owner/b"},
			FileMap:      map[string]string{"owner/b": path},
			WorkTree:     work,
			Home:         filepath.Join(root, "home"),
			GitOf:        git,
		})
		if err == nil {
			t.Errorf("%s: Resolve accepted a checkout git cannot use in-sandbox", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
}

// TestDetectWorkTreeWithholdsTheClaim: the working tree is bound writable
// either way, so these are WARNINGS — but rein must not CLAIM a repo it cannot
// stand behind, because the banner and the agent's REIN_REPO_WORKTREES both
// repeat the claim. A subdir of repo A is NOT repo A's checkout: git cannot
// work there in-sandbox (its .git is outside the bind).
func TestDetectWorkTreeWithholdsTheClaim(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "cmd")
	realRoot := filepath.Join(root, "a")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(Params{
		SessionRepos: []string{"owner/a"},
		WorkTree:     sub,
		Home:         filepath.Join(root, "home"),
		GitOf: fakeGitInfo(map[string]GitInfo{
			sub: {Origin: "https://github.com/owner/a", Root: realRoot}, // git walked UP
		}),
	})
	if err != nil {
		t.Fatalf("a subdir working tree must not fail the launch: %v", err)
	}
	if res.WorkTreeRepo != "" {
		t.Errorf("rein claimed the subdir %s IS owner/a's checkout; it is not (git cannot work there in-sandbox)", sub)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "SUBDIRECTORY") {
		t.Errorf("want a subdirectory warning; got %v", res.Warnings)
	}
	if v := AgentEnvValue(res.AgentBindings(sub)); v != "" {
		t.Errorf("the agent must not be told a false checkout path; got %q", v)
	}

	// A LINKED working tree: same withholding, different reason.
	linked := filepath.Join(root, "lw")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err = Resolve(Params{
		SessionRepos: []string{"owner/a"},
		WorkTree:     linked,
		Home:         filepath.Join(root, "home"),
		GitOf: fakeGitInfo(map[string]GitInfo{
			linked: {Origin: "https://github.com/owner/a", Root: linked, Linked: true},
		}),
	})
	if err != nil {
		t.Fatalf("a linked working tree must not fail the launch: %v", err)
	}
	if res.WorkTreeRepo != "" {
		t.Errorf("a linked worktree must not be claimed as a usable checkout: %q", res.WorkTreeRepo)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "LINKED") {
		t.Errorf("want a linked-worktree warning; got %v", res.Warnings)
	}
}

// TestResolveFailsClosedWithoutHome: the $HOME geometry guard (a mapped path
// must not BE $HOME or an ancestor of it) cannot be skipped just because rein
// could not resolve $HOME. Fail closed — the package's whole contract.
func TestResolveFailsClosedWithoutHome(t *testing.T) {
	root := t.TempDir()
	work := mkCheckout(t, filepath.Join(root, "a"))
	b := mkCheckout(t, filepath.Join(root, "b"))
	_, err := Resolve(Params{
		SessionRepos: []string{"owner/a", "owner/b"},
		FileMap:      map[string]string{"owner/b": b},
		WorkTree:     work,
		Home:         "", // unresolvable
		GitOf: fakeGit(map[string]string{
			work: "https://github.com/owner/a",
			b:    "https://github.com/owner/b",
		}),
	})
	if err == nil {
		t.Fatal("with no home dir, the $HOME geometry guard cannot run — Resolve must refuse, not skip it")
	}
}
