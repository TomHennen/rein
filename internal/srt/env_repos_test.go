package srt

import (
	"strings"
	"testing"
)

// TestBuildEnv_ReposCarriedIn (issue #180): in-sandbox there is no
// session file, so this env var is the only way `rein session show` (or
// an agent that looks) can report the run's scope. Absent when unset,
// never an empty value that reads as "no repos".
func TestBuildEnv_ReposCarriedIn(t *testing.T) {
	env := BuildEnv(EnvParams{
		Parent:       []string{"HOME=/home/dev"},
		CABundlePath: "/run/ca-bundle.pem",
		StubGHToken:  "stub-tok",
		Repos:        "o/r,o/other",
	})
	found := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, EnvInSandboxRepos+"="); ok {
			found = v
		}
	}
	if found != "o/r,o/other" {
		t.Errorf("%s = %q, want %q", EnvInSandboxRepos, found, "o/r,o/other")
	}

	bare := BuildEnv(EnvParams{Parent: []string{"HOME=/home/dev"}, CABundlePath: "/x", StubGHToken: "s"})
	for _, kv := range bare {
		if strings.HasPrefix(kv, EnvInSandboxRepos+"=") {
			t.Errorf("%s must be absent when no repos are supplied; got %q", EnvInSandboxRepos, kv)
		}
	}
}
