package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realGitForTest returns the system git path, skipping the test if git is absent.
func realGitForTest(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	return p
}

func runGit(t *testing.T, dir, git string, args ...string) {
	t.Helper()
	cmd := exec.Command(git, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// captureUpstreamIntent resolves HEAD via real git and records the tracking a
// `-u` push would have set. Exercised against a real repo on a real branch.
func TestCaptureUpstreamIntentWritesLine(t *testing.T) {
	git := realGitForTest(t)
	repo := t.TempDir()
	runGit(t, repo, git, "init", "-q")
	runGit(t, repo, git, "commit", "-q", "--allow-empty", "-m", "init")
	runGit(t, repo, git, "checkout", "-q", "-b", "agent/73/ab12")

	intent := filepath.Join(t.TempDir(), "intent")

	// The shim resolves HEAD in its own cwd; point it at the repo.
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}

	captureUpstreamIntent([]string{"push", "-u", "origin", "HEAD:refs/heads/agent/73/ab12"}, intent, git)

	data, err := os.ReadFile(intent)
	if err != nil {
		t.Fatalf("intent file not written: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := "origin\tagent/73/ab12\trefs/heads/agent/73/ab12"
	if got != want {
		t.Fatalf("intent = %q, want %q", got, want)
	}
}

// Detached HEAD → no branch → capture is skipped, no file created.
func TestCaptureUpstreamIntentDetachedHeadSkips(t *testing.T) {
	git := realGitForTest(t)
	repo := t.TempDir()
	runGit(t, repo, git, "init", "-q")
	runGit(t, repo, git, "commit", "-q", "--allow-empty", "-m", "init")
	// Detach onto the commit.
	out, err := exec.Command(git, "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, git, "checkout", "-q", strings.TrimSpace(string(out)))

	intent := filepath.Join(t.TempDir(), "intent")
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}

	captureUpstreamIntent([]string{"push", "-u"}, intent, git)

	if _, err := os.Stat(intent); !os.IsNotExist(err) {
		t.Fatalf("intent file should not exist on detached HEAD; stat err=%v", err)
	}
}
