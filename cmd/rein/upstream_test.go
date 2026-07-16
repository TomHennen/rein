package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func upstreamGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func gitCfg(t *testing.T, dir, key string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "config", "--get", key).Output()
	if err != nil {
		return "" // unset
	}
	return strings.TrimSpace(string(out))
}

// setupRepoWithRemote makes a bare "origin" and a working clone on branch
// agent/73/ab12, returning the working-tree path.
func setupRepoWithRemote(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	work := filepath.Join(base, "work")
	upstreamGit(t, base, "init", "-q", "--bare", bare)
	upstreamGit(t, base, "clone", "-q", bare, work)
	upstreamGit(t, work, "commit", "-q", "--allow-empty", "-m", "init")
	upstreamGit(t, work, "checkout", "-q", "-b", "agent/73/ab12")
	return work
}

func writeIntent(t *testing.T, work, content string) string {
	t.Helper()
	p := filepath.Join(work, ".git", upstreamIntentBasename)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyUpstreamIntent_SetsTrackingAndRemovesFile(t *testing.T) {
	work := setupRepoWithRemote(t)
	p := writeIntent(t, work, "origin\tagent/73/ab12\trefs/heads/agent/73/ab12\n")

	applyUpstreamIntent(work, p, log.New(io.Discard, "", 0))

	if got := gitCfg(t, work, "branch.agent/73/ab12.remote"); got != "origin" {
		t.Errorf("branch remote = %q, want origin", got)
	}
	if got := gitCfg(t, work, "branch.agent/73/ab12.merge"); got != "refs/heads/agent/73/ab12" {
		t.Errorf("branch merge = %q, want refs/heads/agent/73/ab12", got)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("intent file should be removed; stat err=%v", err)
	}
}

func TestApplyUpstreamIntent_RejectsNonexistentBranch(t *testing.T) {
	work := setupRepoWithRemote(t)
	// Branch "attacker" does not exist locally → must be skipped.
	p := writeIntent(t, work, "origin\tattacker\trefs/heads/attacker\n")

	applyUpstreamIntent(work, p, log.New(io.Discard, "", 0))

	if got := gitCfg(t, work, "branch.attacker.remote"); got != "" {
		t.Errorf("tracking set for nonexistent branch: %q", got)
	}
}

func TestApplyUpstreamIntent_RejectsNonexistentRemote(t *testing.T) {
	work := setupRepoWithRemote(t)
	p := writeIntent(t, work, "evil\tagent/73/ab12\trefs/heads/agent/73/ab12\n")

	applyUpstreamIntent(work, p, log.New(io.Discard, "", 0))

	if got := gitCfg(t, work, "branch.agent/73/ab12.remote"); got != "" {
		t.Errorf("tracking set for nonexistent remote: %q", got)
	}
}

// A forged line trying to reach a code-exec key must not do so: rein only ever
// writes branch.<local>.remote/.merge, and validation rejects the bogus branch,
// so no core.* key is touched.
func TestApplyUpstreamIntent_ForgedLineCannotReachCodeExecKey(t *testing.T) {
	work := setupRepoWithRemote(t)
	// Attempt to smuggle a config path; the local field is validated as a ref
	// name (no spaces/newlines) and must be an existing branch — both fail here.
	p := writeIntent(t, work, "origin\tcore.pager=evil\trefs/heads/x\n")

	applyUpstreamIntent(work, p, log.New(io.Discard, "", 0))

	if got := gitCfg(t, work, "core.pager"); got != "" {
		t.Fatalf("core.pager was set to %q — forged line reached a code-exec key!", got)
	}
}

func TestApplyUpstreamIntent_LastWriteWins(t *testing.T) {
	work := setupRepoWithRemote(t)
	upstreamGit(t, work, "remote", "add", "upstream", filepath.Join(t.TempDir(), "u.git"))
	// Two lines for the same branch; the second should win.
	p := writeIntent(t, work,
		"origin\tagent/73/ab12\trefs/heads/agent/73/ab12\n"+
			"upstream\tagent/73/ab12\trefs/heads/agent/73/ab12\n")

	applyUpstreamIntent(work, p, log.New(io.Discard, "", 0))

	if got := gitCfg(t, work, "branch.agent/73/ab12.remote"); got != "upstream" {
		t.Errorf("last-write-wins failed: remote = %q, want upstream", got)
	}
}

func TestApplyUpstreamIntent_NoFileIsNoop(t *testing.T) {
	work := setupRepoWithRemote(t)
	// Should not panic or error when the rendezvous file is absent.
	applyUpstreamIntent(work, filepath.Join(work, ".git", upstreamIntentBasename), log.New(io.Discard, "", 0))
}

// A forged line must not RETARGET a branch that already has an upstream (the
// clone set branch.main.remote); only fresh branches get tracking.
func TestApplyUpstreamIntent_DoesNotOverwriteExistingUpstream(t *testing.T) {
	work := setupRepoWithRemote(t)
	// Give a branch a real upstream (push -u to the local bare origin), then try
	// to RETARGET it via a forged intent line.
	upstreamGit(t, work, "checkout", "-q", "-b", "tracked")
	upstreamGit(t, work, "push", "-q", "-u", "origin", "tracked")
	if got := gitCfg(t, work, "branch.tracked.remote"); got != "origin" {
		t.Fatalf("precondition: branch.tracked.remote = %q, want origin", got)
	}
	before := gitCfg(t, work, "branch.tracked.merge")
	p := writeIntent(t, work, "origin\ttracked\trefs/heads/evil\n")

	applyUpstreamIntent(work, p, log.New(io.Discard, "", 0))

	if got := gitCfg(t, work, "branch.tracked.merge"); got != before {
		t.Fatalf("existing upstream was RETARGETED: branch.tracked.merge %q -> %q", before, got)
	}
}

// An agent-planted FIFO must be ignored, not opened (os.Open on a FIFO blocks
// forever). The test would hang if the guard regressed.
func TestApplyUpstreamIntent_IgnoresFifo(t *testing.T) {
	work := setupRepoWithRemote(t)
	p := filepath.Join(work, ".git", upstreamIntentBasename)
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	done := make(chan struct{})
	go func() {
		applyUpstreamIntent(work, p, log.New(io.Discard, "", 0))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("applyUpstreamIntent blocked on a FIFO rendezvous (would hang rein post-run)")
	}
}

// A symlink rendezvous must be ignored (never followed/read).
func TestApplyUpstreamIntent_IgnoresSymlink(t *testing.T) {
	work := setupRepoWithRemote(t)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("origin\tagent/73/ab12\trefs/heads/agent/73/ab12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// setupRepoWithRemote already left us on agent/73/ab12 (no upstream).
	p := filepath.Join(work, ".git", upstreamIntentBasename)
	if err := os.Symlink(secret, p); err != nil {
		t.Fatal(err)
	}
	applyUpstreamIntent(work, p, log.New(io.Discard, "", 0))
	if got := gitCfg(t, work, "branch.agent/73/ab12.remote"); got != "" {
		t.Fatalf("symlink rendezvous was followed and applied: %q", got)
	}
}
