package srt

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllowUnhardenedGitFromEnv(t *testing.T) {
	for _, on := range []string{"1", "true", "YES", " on "} {
		if !AllowUnhardenedGitFromEnv(on) {
			t.Errorf("expected %q to opt in", on)
		}
	}
	for _, off := range []string{"", "0", "false", "no", "garbage"} {
		if AllowUnhardenedGitFromEnv(off) {
			t.Errorf("expected %q to stay fail-closed", off)
		}
	}
}

func TestAssessGitHardening(t *testing.T) {
	root := t.TempDir()

	// (1) plain checkout: .git is a dir, no submodules -> fully hardenable, no gap.
	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(filepath.Join(plain, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// (2) submodule-bearing checkout: .git/modules non-empty -> gap.
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(filepath.Join(sub, ".git", "modules", "libfoo"), 0o755); err != nil {
		t.Fatal(err)
	}

	// (3) empty modules dir -> NOT a gap (no submodule gitdirs).
	emptymods := filepath.Join(root, "emptymods")
	if err := os.MkdirAll(filepath.Join(emptymods, ".git", "modules"), 0o755); err != nil {
		t.Fatal(err)
	}

	// (4) linked worktree: .git is a FILE -> gap.
	linked := filepath.Join(root, "linked")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: /somewhere/else\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// (5) non-repo (no .git) -> not a gap (nothing to harden).
	nonrepo := filepath.Join(root, "nonrepo")
	if err := os.MkdirAll(nonrepo, 0o755); err != nil {
		t.Fatal(err)
	}

	gaps := AssessGitHardening([]string{plain, sub, emptymods, linked, nonrepo, ""})
	gapSet := map[string]string{}
	for _, g := range gaps {
		gapSet[g.Tree] = g.Reason
	}

	if _, ok := gapSet[plain]; ok {
		t.Errorf("plain checkout should be fully hardenable (no gap); got %q", gapSet[plain])
	}
	if _, ok := gapSet[emptymods]; ok {
		t.Errorf("empty modules dir should not be a gap; got %q", gapSet[emptymods])
	}
	if _, ok := gapSet[nonrepo]; ok {
		t.Errorf("non-repo should not be a gap; got %q", gapSet[nonrepo])
	}
	if _, ok := gapSet[sub]; !ok {
		t.Errorf("submodule-bearing checkout %q must be flagged as a gap", sub)
	}
	if _, ok := gapSet[linked]; !ok {
		t.Errorf("linked worktree (.git is a file) %q must be flagged as a gap", linked)
	}
}
