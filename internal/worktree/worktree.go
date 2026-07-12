// Package worktree resolves the developer's EXISTING local checkouts of the
// session's repos into validated, writable sandbox bind-mounts (issue #64).
//
// Why this exists (Tom, PR #56): "I had expected it to work in my local copy.
// That makes collaboration easier." Using the developer's real checkout is the
// EXPECTED behavior; the ephemeral clone (docs/session-scope-ux-mocks.md §7) is
// the fallback for a repo that only enters scope MID-RUN — bwrap binds are
// fixed at sandbox launch, so a mid-run scope approval can never make a new
// path writable. Everything this package resolves is therefore known at LAUNCH:
//
//   - the WORKING TREE the developer is standing in (autodetected — its git
//     remote names the repo; see docs/session-scope-ux-mocks.md §3: "detect the
//     repo the user is standing in and make it the default everywhere a repo
//     must be named"), and
//   - the session's `worktrees:` map (repo -> absolute local path), the
//     explicit, human-authored record of where repo B..N live, plus the
//     REIN_WORKTREES per-run override.
//
// Every entry is validated FAIL CLOSED before it becomes a bind: it must be an
// absolute, symlink-resolved, existing directory; it must be a git checkout;
// its `origin` remote must actually be the GitHub repo it is mapped to; and
// that repo must be inside the session's scope ceiling (a writable tree for an
// out-of-scope repo is incoherent — rein would never mint a credential for it).
// A mismatch is an ERROR, never a silent skip: silently NOT binding a tree the
// human named would send the agent off to clone a stale copy, and silently
// binding a tree whose remote we could not confirm would hand a real, possibly
// unrelated directory to a (possibly prompt-injected) agent.
//
// SECURITY (the new exposure this package creates, on record): a mapped
// checkout is bound READ-WRITE. It may hold uncommitted human work, and a
// prompt-injected agent can modify it. That is the deliberate trade for "it
// should just work in my local copy" — the mitigation is that the tree must be
// (a) named by the human (in the session file or the env override) or the cwd
// they launched from, (b) in the session's repo ceiling, and (c) LISTED LOUDLY
// in the launch banner. Nothing is discovered by scanning the filesystem.
package worktree

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TomHennen/rein/internal/brokercore"
	"github.com/TomHennen/rein/internal/proxy"
)

// EnvWorktrees is the per-run override / addition to the session's `worktrees:`
// map: a colon-separated list of `owner/repo=/abs/path` entries (the same
// PATH-style separator convention as REIN_SANDBOX_ALLOW_READ). An entry here
// REPLACES a same-repo entry from the session file, so a developer with two
// checkouts can point one run at the other without editing the session.
const EnvWorktrees = "REIN_WORKTREES"

// The AGENT-visible names for these bindings (REIN_REPO_WORKTREES) and for the
// ephemeral clone dir (REIN_EPHEMERAL_CLONE_DIR) live in internal/srt, which
// owns the sandbox environment. This package renders the VALUE (AgentEnvValue);
// srt sets the variable.

// Binding is one validated local checkout to bind into the sandbox.
type Binding struct {
	// Repo is the canonical "owner/name" this checkout is a checkout OF,
	// confirmed against its git `origin` remote.
	Repo string `json:"repo"`

	// Path is the symlink-resolved absolute host path — which is also the
	// in-sandbox path (srt bind-mounts same-path).
	Path string `json:"path"`

	// Mode is "rw" for every binding today. Present in the agent-visible JSON
	// so a future read-only tier (should Tom want one) is a value change, not a
	// schema change for the agent to re-learn.
	Mode string `json:"mode"`

	// Source records where the mapping came from ("cwd", "session", "env") for
	// the banner. Not exposed to the agent — it is a provenance fact for the
	// human deciding whether the widening is what they meant.
	Source string `json:"-"`
}

// Params are the inputs to Resolve. Paths must be absolute; WorkTree must be
// symlink-resolved already (the caller resolves it before every other use).
type Params struct {
	// SessionRepos is the session's scope ceiling ("owner/name" entries).
	SessionRepos []string

	// FileMap is the session file's `worktrees:` map (repo -> abs path).
	FileMap map[string]string

	// EnvValue is the raw REIN_WORKTREES value (may be empty).
	EnvValue string

	// WorkTree is the resolved working tree (the cwd, or REIN_SANDBOX_WORKDIR).
	// It is ALREADY bound writable by the caller; Resolve only autodetects
	// which session repo it is a checkout of, so the agent can be told.
	WorkTree string

	// Home is the developer's resolved home directory. Used for the "a mapped
	// path must not be $HOME or an ancestor of it" guard — such an entry would
	// bind the whole home tree writable and blow a hole through the #59 model.
	Home string

	// GitOf inspects a directory as a git checkout. Nil means GitInspect (the
	// real thing); tests inject a fake.
	GitOf func(dir string) (GitInfo, error)
}

// GitInfo is what rein needs to know about a directory that claims to be a
// checkout: the `origin` remote it points at, and the checkout ROOT git itself
// reports for that directory.
//
// Root is load-bearing, not decoration. `git -C <dir>` WALKS UP to a parent
// repository, so a SUBDIRECTORY of a checkout answers every remote query
// happily — and rein would then label a subdir as "the repo", bind only that
// subdir writable, and tell the agent it is repo A's checkout. It is not: its
// .git lives outside the bind, so git cannot work there at all. Comparing Root
// to the directory itself is what distinguishes a real checkout root from a
// directory that merely SITS in one.
type GitInfo struct {
	// Origin is the `origin` remote URL ("" when the checkout has no origin).
	Origin string

	// Root is the absolute path git reports as the checkout's top level.
	Root string

	// Linked reports a LINKED git worktree (`git worktree add` — its .git is a
	// FILE pointing at <main-repo>/.git/worktrees/<name>). Its git metadata
	// lives OUTSIDE the tree, so binding the tree alone gives the agent a
	// directory in which every git command fails ("not a git repository").
	Linked bool
}

// Result is Resolve's output.
type Result struct {
	// Bindings are the EXTRA writable binds (the mapped checkouts). The working
	// tree is NOT included — the caller already binds it.
	Bindings []Binding

	// WorkTreeRepo is the session repo the working tree is a checkout of, when
	// autodetection succeeded and it is in scope. Empty otherwise.
	WorkTreeRepo string

	// Warnings are non-fatal human-facing notes (an undetectable or
	// out-of-scope working tree; a mapped path that is redundant because it
	// sits inside the working tree). Printed at launch; never silent.
	Warnings []string
}

// AgentBindings returns every binding the AGENT should be told about: the
// working tree (when its repo was detected) plus the mapped checkouts. This is
// what EnvAgentWorktrees carries.
func (r Result) AgentBindings(workTree string) []Binding {
	out := make([]Binding, 0, len(r.Bindings)+1)
	if r.WorkTreeRepo != "" {
		out = append(out, Binding{Repo: r.WorkTreeRepo, Path: workTree, Mode: "rw", Source: "cwd"})
	}
	out = append(out, r.Bindings...)
	return out
}

// AgentEnvValue renders bindings as the compact JSON array the sandboxed agent
// reads from REIN_REPO_WORKTREES. Returns "" for an empty set (BuildEnv then
// omits the variable rather than setting it to an empty array).
func AgentEnvValue(bs []Binding) string {
	if len(bs) == 0 {
		return ""
	}
	body, err := json.Marshal(bs)
	if err != nil {
		return "" // unreachable for this shape; an unset var is the safe degradation
	}
	return string(body)
}

// ParseEnv parses a REIN_WORKTREES value: colon-separated `owner/repo=/abs/path`
// entries. Empty segments are tolerated; anything malformed is a hard error —
// fail closed rather than guess which tree the operator meant to hand over.
func ParseEnv(v string) (map[string]string, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(v, ":") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		repo, path, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("%s entry %q must be owner/repo=/abs/path", EnvWorktrees, p)
		}
		repo = strings.TrimSpace(repo)
		path = strings.TrimSpace(path)
		if brokercore.RepoFromPath(repo) == "" {
			return nil, fmt.Errorf("%s entry %q: %q is not owner/repo", EnvWorktrees, p, repo)
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("%s entry %q: path must be absolute (no ~ or relative paths; fail closed rather than guess)", EnvWorktrees, p)
		}
		key := strings.ToLower(brokercore.RepoFromPath(repo))
		if prev, dup := out[key]; dup {
			return nil, fmt.Errorf("%s maps %s twice (%q and %q)", EnvWorktrees, key, prev, path)
		}
		out[key] = filepath.Clean(path)
	}
	return out, nil
}

// Resolve validates the session/env worktree map into writable bindings and
// autodetects the working tree's repo. Every failure is fatal to the launch:
// see the package doc for why a silent skip is the wrong degradation.
//
// The one deliberate NON-error is the WORKING TREE's own repo: it is bound
// writable today regardless of what its remote says (that is the pre-#64
// behavior, and `rein run` outside a git checkout is legal), so an undetectable
// or out-of-scope working-tree repo produces a WARNING, not a refusal. Mapped
// entries — which the human explicitly named — are held to the strict rule.
func Resolve(p Params) (Result, error) {
	gitOf := p.GitOf
	if gitOf == nil {
		gitOf = GitInspect
	}
	var res Result

	// (1) The working tree: autodetect which session repo it is (mocks §3).
	//
	// Every failure here is a WARNING, never a refusal: the working tree is
	// bound writable regardless (pre-#64 behavior), and `rein run` outside a
	// checkout is legal. What must never happen is rein CLAIMING a repo it
	// cannot stand behind — the banner and the agent env would both be lying.
	res.Warnings = append(res.Warnings, p.detectWorkTree(gitOf, &res)...)

	// (2) The explicit map: session file, then env overrides on top.
	envMap, err := ParseEnv(p.EnvValue)
	if err != nil {
		return Result{}, err
	}
	merged := map[string]string{}
	sources := map[string]string{}
	for repo, path := range p.FileMap {
		key := strings.ToLower(brokercore.RepoFromPath(repo))
		if key == "" {
			return Result{}, fmt.Errorf("session worktrees: key %q is not owner/repo", repo)
		}
		if !filepath.IsAbs(path) {
			return Result{}, fmt.Errorf("session worktrees[%s] = %q must be an absolute path", repo, path)
		}
		merged[key] = filepath.Clean(path)
		sources[key] = "session"
	}
	for key, path := range envMap {
		merged[key] = path
		sources[key] = "env"
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic banner/env/error order (map order is not)

	for _, repo := range keys {
		path := merged[repo]

		// (2a) The repo must be inside the session's scope ceiling. A writable
		// tree for a repo rein will never mint a credential for is incoherent:
		// the agent could commit but never push, and the human would have
		// widened the filesystem without widening the scope they reviewed.
		match := matchScope(repo, p.SessionRepos)
		if match == "" {
			return Result{}, fmt.Errorf("worktrees maps %s -> %s, but %s is not in this session's repos (%s); add it to the session first — binding a writable tree for an out-of-scope repo would give the agent a tree it can never push",
				repo, path, repo, strings.Join(p.SessionRepos, ", "))
		}

		// (2b) The path must exist, be a directory, and resolve. A dangling
		// symlink / missing dir is an error, not a skip: srt silently drops a
		// nonexistent allowWrite source, so the run would look fine and the
		// agent would find nothing there.
		resolved, err := proxy.ResolveAbs(path)
		if err != nil {
			return Result{}, fmt.Errorf("worktrees[%s] = %q: resolve: %w", repo, path, err)
		}
		fi, err := os.Stat(resolved)
		if err != nil {
			return Result{}, fmt.Errorf("worktrees[%s] = %q does not exist (%v) — a missing bind source is silently dropped by the sandbox, so this fails the launch instead", repo, path, err)
		}
		if !fi.IsDir() {
			return Result{}, fmt.Errorf("worktrees[%s] = %q is not a directory", repo, path)
		}

		// (2c) Geometry. Reject anything that would widen far beyond one repo.
		if p.Home == "" {
			return Result{}, fmt.Errorf("worktrees[%s]: cannot resolve the home directory to check that %q is not it (or an ancestor of it); refusing to bind a tree rein cannot place", repo, resolved)
		}
		if resolved == "/" || resolved == p.Home || pathWithin(p.Home, resolved) {
			return Result{}, fmt.Errorf("worktrees[%s] = %q is your home directory or an ancestor of it; binding it writable would hand the agent your whole home tree (issue #59) — point it at the repo checkout itself", repo, resolved)
		}
		if pathWithin(p.WorkTree, resolved) && resolved != p.WorkTree {
			return Result{}, fmt.Errorf("worktrees[%s] = %q CONTAINS the working tree %s; binding a parent of the working tree writable widens far past one repo — point it at the checkout itself", repo, resolved, p.WorkTree)
		}
		// (2d) It must actually BE a checkout of the repo it is mapped to.
		// This runs BEFORE the "already writable" skip below, deliberately: a
		// mapping that points at the WRONG tree is an error the human must see
		// even when that tree happens to be the working tree (where the skip
		// would otherwise swallow it and leave the human believing repo B is
		// mapped when it is not).
		// Fail closed on anything else — a mis-mapped tree is how an agent
		// scoped to repo B ends up rewriting an unrelated project.
		if !isGitCheckout(resolved) {
			return Result{}, fmt.Errorf("worktrees[%s] = %q is not a git checkout (no .git); refusing to bind it writable", repo, resolved)
		}
		info, err := gitOf(resolved)
		if err != nil {
			return Result{}, fmt.Errorf("worktrees[%s] = %q: cannot read it as a git checkout (%v); refusing to bind a tree whose identity rein cannot confirm", repo, resolved, err)
		}
		// A LINKED git worktree keeps its metadata in the MAIN repo's
		// .git/worktrees/<name>, OUTSIDE this bind — and under the #59 home deny
		// that path is tmpfs. Binding it would hand the agent a tree where every
		// git command dies with "not a git repository" (reproduced on srt 0.0.63)
		// while the banner claims repo B is ready to work in: a fail-OPEN into a
		// broken run. Refuse, and say what to map instead.
		if info.Linked {
			return Result{}, fmt.Errorf("worktrees[%s] = %q is a LINKED git worktree (`git worktree add`); its git metadata lives outside the tree, so git cannot work there inside the sandbox — map the MAIN checkout of %s instead", repo, resolved, repo)
		}
		// It must be the checkout ROOT, not a subdirectory of one: `git -C` walks
		// UP, so a subdir answers every remote query while its .git sits outside
		// the bind (git then fails in-sandbox).
		if root, rerr := proxy.ResolveAbs(info.Root); rerr != nil || root != resolved {
			return Result{}, fmt.Errorf("worktrees[%s] = %q is not the root of the checkout (its git root is %s); map the checkout root, or git cannot work there inside the sandbox", repo, resolved, info.Root)
		}
		canon := repoFromRemoteOrEmpty(info.Origin)
		if canon == "" {
			return Result{}, fmt.Errorf("worktrees[%s] = %q: its origin remote %q is not a github.com repo; refusing to bind it writable", repo, resolved, info.Origin)
		}
		if !strings.EqualFold(canon, repo) {
			return Result{}, fmt.Errorf("worktrees[%s] = %q is a checkout of %s, NOT %s (origin: %s); refusing to bind the wrong tree writable — fix the path or the mapping",
				repo, resolved, canon, repo, info.Origin)
		}

		// (2e) Only now: is this bind redundant? A tree that IS the working tree
		// (or sits inside it) is already writable. Not an error — but never
		// silent, or the human cannot tell a redundant entry from a live bind.
		if resolved == p.WorkTree || pathWithin(resolved, p.WorkTree) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("worktrees[%s] = %s is the working tree (or inside it) and is already writable; ignoring the entry", repo, resolved))
			continue
		}

		res.Bindings = append(res.Bindings, Binding{Repo: match, Path: resolved, Mode: "rw", Source: sources[repo]})
	}

	// (2f) No mapped path may nest inside another (or duplicate it): overlapping
	// writable binds mean one repo's tree lives inside another's, which srt would
	// happily bind twice and git would see as a nested repo. srt.Build checks
	// widening-vs-DENY, never widening-vs-widening, so this check is ours.
	for i := range res.Bindings {
		for j := range res.Bindings {
			if i == j {
				continue
			}
			if pathWithin(res.Bindings[i].Path, res.Bindings[j].Path) {
				return Result{}, fmt.Errorf("worktrees: %s (%s) is nested inside %s (%s); mapped checkouts must not overlap",
					res.Bindings[i].Repo, res.Bindings[i].Path, res.Bindings[j].Repo, res.Bindings[j].Path)
			}
		}
	}
	return res, nil
}

// detectWorkTree autodetects the session repo the working tree is a checkout
// of, setting res.WorkTreeRepo only when rein can stand behind the claim. It
// returns the warnings to surface.
//
// The bar is deliberately high, because BOTH the human banner and the agent's
// REIN_REPO_WORKTREES repeat this claim. Three ways it is withheld:
//
//   - the directory is not a checkout root (`git -C` walks UP, so a SUBDIR of
//     repo A answers the remote query — but only the subdir is bound, its .git
//     is outside the bind, and git cannot work there in-sandbox);
//   - it is a LINKED worktree (metadata outside the tree; same breakage);
//   - its repo is not in the session's scope ceiling.
//
// Each is a warning, not a refusal: the working tree is bound writable either
// way (pre-#64 behavior), and refusing here would regress runs that work today.
func (p Params) detectWorkTree(gitOf func(string) (GitInfo, error), res *Result) []string {
	info, err := gitOf(p.WorkTree)
	if err != nil {
		return []string{fmt.Sprintf("could not detect a GitHub repo for the working tree %s (%v); the agent is not told which repo it is standing in", p.WorkTree, err)}
	}
	if info.Linked {
		return []string{fmt.Sprintf("the working tree %s is a LINKED git worktree; its git metadata lives outside the tree, so git will NOT work there inside the sandbox — launch from the main checkout instead", p.WorkTree)}
	}
	if root, rerr := proxy.ResolveAbs(info.Root); rerr != nil || root != p.WorkTree {
		return []string{fmt.Sprintf("the working tree %s is a SUBDIRECTORY of the checkout at %s, not its root — only this subdirectory is bound writable and git will NOT work in it inside the sandbox; launch from %s instead", p.WorkTree, info.Root, info.Root)}
	}
	canon := repoFromRemoteOrEmpty(info.Origin)
	if canon == "" {
		return []string{fmt.Sprintf("working tree %s has a non-GitHub origin remote (%s); the agent is not told which repo it is standing in", p.WorkTree, info.Origin)}
	}
	match := matchScope(canon, p.SessionRepos)
	if match == "" {
		return []string{fmt.Sprintf("working tree %s is a checkout of %s, which is NOT in this session's scope (%s) — the agent can edit it but cannot push it", p.WorkTree, canon, strings.Join(p.SessionRepos, ", "))}
	}
	res.WorkTreeRepo = match
	return nil
}

// isGitCheckout reports whether dir has a .git entry (a directory for a normal
// clone, a FILE for a linked `git worktree` — both are real checkouts).
func isGitCheckout(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// GitInspect reads the `origin` remote, the checkout root, and the linked-worktree
// flag for dir.
//
// It shells out to git (rather than parsing .git/config) so `insteadOf`
// rewrites and worktree/submodule layouts resolve exactly as they do for the
// developer. The environment is minimized: no global/system config, no
// credential prompts, no askpass — `config --get` and `rev-parse` run no hooks
// and no aliases, so a hostile repo cannot turn this into code execution.
func GitInspect(dir string) (GitInfo, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + os.Getenv("HOME"),
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_TERMINAL_PROMPT=0",
			"GIT_ASKPASS=true",
		}
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git -C %s %s: %w", dir, strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return GitInfo{}, err
	}
	origin, err := git("config", "--get", "remote.origin.url")
	if err != nil {
		return GitInfo{}, fmt.Errorf("%s has no `origin` remote: %w", dir, err)
	}
	if origin == "" {
		return GitInfo{}, fmt.Errorf("%s has no `origin` remote", dir)
	}
	// A linked worktree's .git is a FILE ("gitdir: <main>/.git/worktrees/<n>").
	linked := false
	if fi, serr := os.Stat(filepath.Join(dir, ".git")); serr == nil && !fi.IsDir() {
		linked = true
	}
	return GitInfo{Origin: origin, Root: root, Linked: linked}, nil
}

// RepoFromRemoteURL extracts the canonical "owner/name" from a github.com git
// remote URL. It accepts the four forms git emits/accepts:
//
//	https://github.com/owner/name(.git)     (with or without userinfo)
//	http://github.com/owner/name(.git)
//	ssh://git@github.com/owner/name(.git)
//	git@github.com:owner/name(.git)         (scp-like)
//
// It returns "" for anything else — INCLUDING a non-github.com host. That is
// load-bearing: a remote at evil.example.com/owner/name must never satisfy the
// "is this a checkout of owner/name?" test, or a mapped path could smuggle an
// unrelated tree past validation under a session repo's name.
func RepoFromRemoteURL(remote string) (string, error) {
	u := strings.TrimSpace(remote)
	if u == "" {
		return "", fmt.Errorf("empty remote URL")
	}
	var host, path string
	switch {
	case strings.Contains(u, "://"):
		scheme, rest, _ := strings.Cut(u, "://")
		switch strings.ToLower(scheme) {
		case "https", "http", "ssh", "git":
		default:
			return "", fmt.Errorf("unsupported remote scheme %q in %q", scheme, remote)
		}
		hostPart, p, ok := strings.Cut(rest, "/")
		if !ok {
			return "", fmt.Errorf("remote %q has no repo path", remote)
		}
		// Strip userinfo (git@, or a leaked user:token@) and any :port.
		if _, after, found := strings.Cut(hostPart, "@"); found {
			hostPart = after
		}
		if h, _, found := strings.Cut(hostPart, ":"); found {
			hostPart = h
		}
		host, path = hostPart, p
	case strings.Contains(u, ":"):
		// scp-like: [user@]host:path
		hostPart, p, _ := strings.Cut(u, ":")
		if _, after, found := strings.Cut(hostPart, "@"); found {
			hostPart = after
		}
		host, path = hostPart, p
	default:
		return "", fmt.Errorf("remote %q is not a URL (a local-path remote cannot identify a GitHub repo)", remote)
	}
	if !strings.EqualFold(strings.TrimSuffix(host, "."), "github.com") {
		return "", fmt.Errorf("remote %q is not on github.com (host %q)", remote, host)
	}
	repo := brokercore.RepoFromPath(path)
	if repo == "" {
		return "", fmt.Errorf("remote %q does not name an owner/repo", remote)
	}
	return repo, nil
}

func repoFromRemoteOrEmpty(remote string) string {
	r, err := RepoFromRemoteURL(remote)
	if err != nil {
		return ""
	}
	return r
}

// matchScope returns the SESSION's spelling of repo (case preserved as the
// human wrote it) when repo is in the ceiling, else "".
func matchScope(repo string, sessionRepos []string) string {
	want := strings.ToLower(brokercore.RepoFromPath(repo))
	if want == "" {
		return ""
	}
	for _, r := range sessionRepos {
		if strings.ToLower(brokercore.RepoFromPath(r)) == want {
			return brokercore.RepoFromPath(r)
		}
	}
	return ""
}

// pathWithin reports whether child equals parent or is nested under it (same
// semantics as srt.pathWithin; both are cleaned, comparison is segment-aware).
func pathWithin(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
