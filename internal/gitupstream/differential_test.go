package gitupstream

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This is the answer to "how do we know ParsePush matches git?": for each push
// argv form, run the REAL `git push -u` into a throwaway repo, read what git
// actually wrote to branch.<x>.remote/.merge, and assert ParsePush produced the
// same. Git is the oracle — a divergence is a bug in ParsePush, not the test.

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitOK runs git and returns its trimmed stdout, or "" on any error (for the
// oracle reads, where an unset key is a legitimate empty result).
func gitOK(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestParsePush_MatchesRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	cases := []struct {
		name     string
		localBr  string   // branch to check out before pushing
		pushArgs []string // argv passed to real git AND to ParsePush (starts with "push")
	}{
		{"HEAD:full-ref", "agent/73/ab", []string{"push", "-u", "origin", "HEAD:refs/heads/agent/73/ab"}},
		{"bare name", "feature", []string{"push", "-u", "origin", "feature"}},
		{"HEAD:short-dst", "work", []string{"push", "-u", "origin", "HEAD:renamed"}},
		{"src:dst", "local", []string{"push", "-u", "origin", "local:remote"}},
		{"set-upstream long flag", "b1", []string{"push", "--set-upstream", "origin", "b1"}},
		{"force marker", "b2", []string{"push", "-u", "origin", "+b2"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			bare := filepath.Join(base, "origin.git")
			work := filepath.Join(base, "work")
			git(t, base, "init", "-q", "--bare", bare)
			git(t, base, "clone", "-q", bare, work)
			git(t, work, "commit", "-q", "--allow-empty", "-m", "init")
			git(t, work, "checkout", "-q", "-b", c.localBr)
			git(t, work, "commit", "-q", "--allow-empty", "-m", "work")

			// Ground truth: real git writes the upstream into the (writable) config.
			git(t, work, c.pushArgs...)
			cur := gitOK(work, "symbolic-ref", "--short", "HEAD")
			gitRemote := gitOK(work, "config", "--get", "branch."+cur+".remote")
			gitMerge := gitOK(work, "config", "--get", "branch."+cur+".merge")
			if gitRemote == "" || gitMerge == "" {
				t.Fatalf("real git set no upstream for %q (remote=%q merge=%q) — case setup is wrong",
					cur, gitRemote, gitMerge)
			}

			// Our parse, given the same argv and git's own HEAD resolution.
			got, ok := ParsePush(c.pushArgs, func() (string, error) { return cur, nil })
			if !ok {
				t.Fatalf("ParsePush declined an argv real git accepted: %v", c.pushArgs)
			}
			if got.Local != cur {
				t.Errorf("Local = %q, git tracked branch %q", got.Local, cur)
			}
			if got.Remote != gitRemote {
				t.Errorf("Remote = %q, git wrote %q", got.Remote, gitRemote)
			}
			if got.Merge != gitMerge {
				t.Errorf("Merge = %q, git wrote %q", got.Merge, gitMerge)
			}
		})
	}
}
