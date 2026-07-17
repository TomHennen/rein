// Host-side apply of the `git push -u` tracking the rein-git shim recorded
// (#102/#119). Untrusted input (see internal/gitupstream); the host-side defenses
// here: read only a plain regular file (a planted FIFO/symlink is ignored, never
// blocked on), and set tracking only on a branch that has none yet (so a forged
// line can't retarget an existing branch).
package main

import (
	"bufio"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/TomHennen/rein/internal/gitupstream"
)

// maxIntentBytes caps how much of the rendezvous file rein reads — a defensive
// bound against an agent bloating it. Real runs write a handful of short lines.
const maxIntentBytes = 64 << 10

// upstreamIntentBasename is the rendezvous file name under the checkout's .git.
const upstreamIntentBasename = "rein-upstream-intent"

// applyUpstreamIntent reads the rendezvous file at intentFile, sets the validated
// branch tracking on the repo at workTree, and removes the file. Every step is
// best-effort: a missing file is the normal no-push case, and any error is logged
// and swallowed (the push already succeeded).
func applyUpstreamIntent(workTree, intentFile string, logger *log.Logger) {
	fi, err := os.Lstat(intentFile)
	if err != nil {
		return // normal: the agent never ran `git push -u`
	}
	// Always unlink the name at the end (removes a symlink/FIFO node itself, never
	// a target), so a stale or planted file can't reapply on a later run.
	defer os.Remove(intentFile)
	if !fi.Mode().IsRegular() {
		// The agent can write .git/, so it could plant a FIFO here (os.Open would
		// block rein forever, post-run) or a symlink (arbitrary-file read). Ignore
		// anything that isn't a plain file.
		logger.Printf("git upstream: rendezvous is not a regular file; ignoring")
		return
	}
	f, err := os.OpenFile(intentFile, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer f.Close()

	g := &repoGit{dir: workTree}

	// Last-write-wins per branch (the agent's final push), first-seen order for
	// stable logs. LimitedReader caps bytes; a truncated line just fails Validate.
	latest := map[string]gitupstream.Intent{}
	var order []string
	sc := bufio.NewScanner(&io.LimitedReader{R: f, N: maxIntentBytes})
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		in, err := gitupstream.ParseLine(line)
		if err != nil {
			logger.Printf("git upstream: skipping malformed intent line: %v", err)
			continue
		}
		if _, ok := latest[in.Local]; !ok {
			order = append(order, in.Local)
		}
		latest[in.Local] = in
	}

	for _, local := range order {
		in := latest[local]
		if !gitupstream.Validate(in, g.remoteExists, g.branchExists) {
			logger.Printf("git upstream: skipping unverifiable intent for branch %q", in.Local)
			continue
		}
		// Only ADD tracking to a branch that has none — matches `push -u`'s real
		// effect on a fresh branch, and denies a forged line the power to RETARGET
		// an existing branch's upstream (e.g. point main at origin/evil).
		if g.hasUpstream(in.Local) {
			logger.Printf("git upstream: branch %q already has an upstream; leaving as-is", in.Local)
			continue
		}
		if err := g.setTracking(in); err != nil {
			logger.Printf("git upstream: setting tracking for %q failed: %v", in.Local, err)
			continue
		}
		logger.Printf("git upstream: set branch.%s tracking -> %s/%s", in.Local, in.Remote, strings.TrimPrefix(in.Merge, "refs/heads/"))
	}
}

// repoGit runs git in a specific working tree.
type repoGit struct{ dir string }

func (g *repoGit) run(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.dir
	return cmd.Output()
}

func (g *repoGit) remoteExists(name string) bool {
	// `git remote get-url` exits non-zero for an unknown remote.
	_, err := g.run("remote", "get-url", name)
	return err == nil
}

func (g *repoGit) branchExists(name string) bool {
	_, err := g.run("rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

// hasUpstream reports whether the branch already has an upstream remote set.
// name is a Validated ref (no leading '-'), so it can't be read as an option.
func (g *repoGit) hasUpstream(name string) bool {
	out, err := g.run("config", "--get", "branch."+name+".remote")
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// setTracking writes ONLY branch.<local>.remote/.merge, via `git config`. The
// key is built from an already-Validated branch name, so no other key is reachable.
func (g *repoGit) setTracking(in gitupstream.Intent) error {
	if _, err := g.run("config", "branch."+in.Local+".remote", in.Remote); err != nil {
		return err
	}
	_, err := g.run("config", "branch."+in.Local+".merge", in.Merge)
	return err
}
