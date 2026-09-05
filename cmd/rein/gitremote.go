// cwd repo autodetection (issue #69, mocks §3): "detect the repo the user
// is standing in and make it the default everywhere a repo must be named."
//
// This is UX only — it supplies a DEFAULT, never a grant. Nothing here
// widens a scope ceiling: `rein init` offers the detected repo as the
// default answer to a question the human still confirms, and `rein run`
// only uses it to say something useful when the cwd's repo isn't in the
// session (instead of a cold "no session" error).
package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/brokercore"
)

// detectRepoTimeout bounds the `git` call — a wedged filesystem or a
// credential-prompting remote must never hang a launch on a nicety.
const detectRepoTimeout = 3 * time.Second

// detectRepoFromGit returns the GitHub "owner/name" of the repo whose work
// tree contains dir, from its `origin` remote. It returns "" — never an
// error — for every "no answer" case, because every caller's fallback is
// simply "no default":
//
//   - dir is not inside a git work tree,
//   - there is no `origin` remote,
//   - `origin` is not a github.com remote (a GitLab/self-hosted URL must not
//     silently become a GitHub repo name),
//   - git is missing or slow.
//
// Both GitHub URL shapes are handled: https://github.com/o/n(.git) and
// git@github.com:o/n(.git).
func detectRepoFromGit(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), detectRepoTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return repoFromRemoteURL(strings.TrimSpace(string(out)))
}

// repoFromRemoteURL maps a git remote URL to "owner/name", or "" if it is
// not a github.com remote. Split out from detectRepoFromGit so it is
// testable without a git checkout.
func repoFromRemoteURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	var path string
	switch {
	case strings.HasPrefix(u, "git@github.com:"):
		path = strings.TrimPrefix(u, "git@github.com:")
	case strings.HasPrefix(u, "ssh://git@github.com/"):
		path = strings.TrimPrefix(u, "ssh://git@github.com/")
	case strings.HasPrefix(u, "https://github.com/"):
		path = strings.TrimPrefix(u, "https://github.com/")
	case strings.HasPrefix(u, "http://github.com/"):
		path = strings.TrimPrefix(u, "http://github.com/")
	default:
		// Includes https://gitlab.com/..., git@github.example.com:... (an
		// enterprise host is NOT github.com), and anything unparseable.
		return ""
	}
	// Strip a userinfo-free host suffix already removed; normalize the rest
	// with the same parser the scope checks use (drops ".git", slashes, and
	// any trailing path segments).
	repo := brokercore.RepoFromPath(path)
	if strings.Count(repo, "/") != 1 {
		return ""
	}
	return repo
}

// noSessionHint augments the cold "no session" failure with the repo the
// human is standing in — the #69 mocks' replacement for that dead end.
func noSessionHint(cwd string) string {
	repo := detectRepoFromGit(cwd)
	if repo == "" {
		return "run `rein init` to create one"
	}
	return fmt.Sprintf("run `rein init --repo %s` to scaffold a session for the repo you are standing in", repo)
}
