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

// TestCwdScopeNotice: the launch-time out-of-scope notice names the repo AND
// both remedies. It was defined-but-called-from-nowhere until this test's PR,
// so the test also keeps it wired.
func TestCwdScopeNotice(t *testing.T) {
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

	sess := session.Session{ID: "s", Role: "implement", Repos: []string{"owner/in"}}
	got := cwdScopeNotice(sess, dir)
	for _, want := range []string{"owner/out", "rein declare <n> --repo owner/out", "rein session add-repo owner/out"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q:\n%s", want, got)
		}
	}

	inScope := session.Session{ID: "s", Role: "implement", Repos: []string{"owner/out"}}
	if n := cwdScopeNotice(inScope, dir); n != "" {
		t.Errorf("an in-scope cwd must say nothing, got %q", n)
	}
	if n := cwdScopeNotice(sess, t.TempDir()); n != "" {
		t.Errorf("a non-checkout cwd must say nothing, got %q", n)
	}
}
