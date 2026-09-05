package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/session"
)

func TestRepoFromRemoteURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/TomH/repo.git":     "TomH/repo",
		"https://github.com/TomH/repo":         "TomH/repo",
		"git@github.com:TomH/repo.git":         "TomH/repo",
		"git@github.com:TomH/repo":             "TomH/repo",
		"ssh://git@github.com/TomH/repo.git":   "TomH/repo",
		"http://github.com/TomH/repo":          "TomH/repo",
		"https://gitlab.com/TomH/repo.git":     "", // not github.com
		"git@github.example.com:TomH/repo.git": "", // enterprise host is not github.com
		"":                                     "",
		"not a url":                            "",
		"https://github.com/TomH":              "", // no repo half
	}
	for in, want := range cases {
		if got := repoFromRemoteURL(in); got != want {
			t.Errorf("repoFromRemoteURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEnsureWorkTreeInScope: an out-of-scope working tree refuses the launch
// (with both remedies) unless the human accepts the add; in-scope and
// non-checkout dirs pass silently.
func TestEnsureWorkTreeInScope(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://github.com/owner/out"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	no := func(string) bool { return false }

	sess := session.Session{ID: "s", Role: "implement", Repos: []string{"owner/in"}}
	err := ensureWorkTreeInScope(&sess, "file:/nonexistent", dir, no)
	if err == nil {
		t.Fatal("out-of-scope working tree + declined prompt must refuse the launch")
	}
	for _, want := range []string{"owner/out", "rein session add-repo owner/out"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q:\n%s", want, err)
		}
	}

	inScope := session.Session{ID: "s", Role: "implement", Repos: []string{"owner/out"}}
	if err := ensureWorkTreeInScope(&inScope, "", dir, no); err != nil {
		t.Errorf("in-scope working tree must pass: %v", err)
	}
	if err := ensureWorkTreeInScope(&sess, "", t.TempDir(), no); err != nil {
		t.Errorf("non-checkout working tree must pass: %v", err)
	}
}
