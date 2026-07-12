package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRepo(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"owner/repo", "owner/repo"},
		{"Owner/Repo", "owner/repo"},
		{"owner/repo.git", "owner/repo"},
		{"owner/repo/", "owner/repo"},
		{"owner/repo.git/", "owner/repo"},
		{"owner/Repo.GIT", "owner/repo"},
		{"owner/repo/info/refs", "owner/repo"}, // sub-path → first two segments
		{"owner/", ""},
		{"/repo", ""},
		{"justastring", ""},
		{"", ""},
		{"  owner/repo  ", "owner/repo"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := normalizeRepo(tc.in)
			if got != tc.want {
				t.Errorf("normalizeRepo(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBareRepoNames(t *testing.T) {
	// Spellings that Validate accepts but a raw strings.Cut would mangle into a
	// name GitHub 422s at mint (issue #10 F2). All must yield the clean name.
	s := &Session{Repos: []string{
		"owner/name",
		"owner/name.git",
		"/owner/name2",
		"owner/name3/",
	}}
	got := s.BareRepoNames()
	want := []string{"name", "name", "name2", "name3"}
	if len(got) != len(want) {
		t.Fatalf("BareRepoNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("BareRepoNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestValidateRejectsMixedOwners(t *testing.T) {
	s := &Session{ID: "s", Repos: []string{"alice/x", "bob/y"}}
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted a mixed-owner session; want rejection")
	}
	ok := &Session{ID: "s", Repos: []string{"alice/x", "Alice/y.git", "/alice/z"}}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate rejected a legitimate single-owner session: %v", err)
	}
}

func TestSessionContains(t *testing.T) {
	s := &Session{Repos: []string{"TomHennen/agentcreds-validation-a", "Other/Repo"}}
	cases := []struct {
		repo string
		want bool
	}{
		{"TomHennen/agentcreds-validation-a", true},
		{"tomhennen/agentcreds-validation-a", true},
		{"TomHennen/agentcreds-validation-a.git", true},
		{"TomHennen/agentcreds-validation-a/", true},
		{"TomHennen/agentcreds-validation-a/info/refs", true}, // sub-path
		{"TomHennen/agentcreds-validation-b", false},
		{"Other/Repo", true},
		{"OTHER/REPO", true},
		{"Other/Repo-Different", false},
		{"", false},
		{"justastring", false},
	}
	for _, tc := range cases {
		t.Run(tc.repo, func(t *testing.T) {
			got := s.Contains(tc.repo)
			if got != tc.want {
				t.Errorf("Contains(%q) = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Run("good", func(t *testing.T) {
		s := Session{ID: "sess_x", Repos: []string{"o/r"}}
		if err := s.Validate(); err != nil {
			t.Errorf("Validate: %v", err)
		}
	})
	t.Run("missing id", func(t *testing.T) {
		s := Session{Repos: []string{"o/r"}}
		if err := s.Validate(); err == nil {
			t.Error("expected error for missing id")
		}
	})
	t.Run("empty repos", func(t *testing.T) {
		s := Session{ID: "x"}
		if err := s.Validate(); err == nil {
			t.Error("expected error for empty repos")
		}
	})
	t.Run("bad repo entry", func(t *testing.T) {
		s := Session{ID: "x", Repos: []string{"not-owner-slash-name"}}
		if err := s.Validate(); err == nil {
			t.Error("expected error for malformed repo entry")
		}
	})
}

func TestLoadFromFile(t *testing.T) {
	t.Run("missing file → os.ErrNotExist", func(t *testing.T) {
		_, err := LoadFromFile(filepath.Join(t.TempDir(), "absent.yaml"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected os.ErrNotExist, got %v", err)
		}
	})
	t.Run("valid YAML round trip", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.yaml")
		body := `
id: sess_dev_001
role: implement
repos:
  - TomHennen/agentcreds-validation-a
`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		s, err := LoadFromFile(p)
		if err != nil {
			t.Fatalf("LoadFromFile: %v", err)
		}
		if s.ID != "sess_dev_001" || s.Role != "implement" || len(s.Repos) != 1 || s.Repos[0] != "TomHennen/agentcreds-validation-a" {
			t.Errorf("got %+v", s)
		}
		if s.Issue != 0 {
			t.Errorf("Issue should default to 0 when omitted, got %d", s.Issue)
		}
	})

	t.Run("session with issue field (CP5)", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "issue.yaml")
		body := "id: x\nrole: implement\nrepos:\n  - o/r\nissue: 73\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		s, err := LoadFromFile(p)
		if err != nil {
			t.Fatalf("LoadFromFile: %v", err)
		}
		if s.Issue != 73 {
			t.Errorf("Issue = %d, want 73", s.Issue)
		}
	})
	t.Run("session with allow_domains field (CP4.5)", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "egress.yaml")
		body := "id: x\nrole: implement\nrepos:\n  - o/r\nallow_domains:\n  - registry.npmjs.org\n  - pypi.org\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		s, err := LoadFromFile(p)
		if err != nil {
			t.Fatalf("LoadFromFile: %v", err)
		}
		if len(s.AllowDomains) != 2 || s.AllowDomains[0] != "registry.npmjs.org" || s.AllowDomains[1] != "pypi.org" {
			t.Errorf("AllowDomains = %v, want [registry.npmjs.org pypi.org]", s.AllowDomains)
		}
	})
	t.Run("malformed YAML → error", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "bad.yaml")
		if err := os.WriteFile(p, []byte("id: x\nrepos: not-a-list\n"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		_, err := LoadFromFile(p)
		if err == nil {
			t.Error("expected error for malformed YAML")
		}
	})
	t.Run("missing id → validation error", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "noid.yaml")
		if err := os.WriteFile(p, []byte("repos:\n  - o/r\n"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		_, err := LoadFromFile(p)
		if err == nil {
			t.Error("expected validation error for missing id")
		}
	})
}

func TestDefaultFilePath(t *testing.T) {
	t.Run("XDG_CONFIG_HOME respected", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/custom/xdg")
		got, err := DefaultFilePath()
		if err != nil {
			t.Fatal(err)
		}
		want := "/custom/xdg/rein/dev-session.yaml"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestLoadOrFallback_ExplicitMissingFileHardErrors(t *testing.T) {
	// REIN_SESSION_FILE set to a path that doesn't exist must be a hard
	// error, NOT a silent env-fallback (the footgun that masked a missing
	// session file as a wrong-scope run).
	t.Setenv("REIN_SESSION_FILE", filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	_, _, err := LoadOrFallback("owner/repo")
	if err == nil {
		t.Fatal("expected hard error for missing REIN_SESSION_FILE, got nil (silent fallback)")
	}
	if !strings.Contains(err.Error(), "REIN_SESSION_FILE") {
		t.Errorf("error should name REIN_SESSION_FILE, got: %v", err)
	}
}

func TestLoadOrFallback_DefaultMissingFallsBackToEnv(t *testing.T) {
	// With REIN_SESSION_FILE UNSET and the default file absent, the Phase 0
	// env-fallback still applies (unchanged behavior).
	t.Setenv("REIN_SESSION_FILE", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // default dev-session.yaml won't exist here
	s, source, err := LoadOrFallback("owner/repo")
	if err != nil {
		t.Fatalf("expected env-fallback, got error: %v", err)
	}
	if source != "env-fallback" {
		t.Errorf("source = %q, want env-fallback", source)
	}
	if len(s.Repos) != 1 || s.Repos[0] != "owner/repo" {
		t.Errorf("fallback repos = %v, want [owner/repo]", s.Repos)
	}
}

// TestWorktreesValidation (#64): the `worktrees:` map is the human's explicit
// statement of "bind MY checkout of this repo writable". Structural nonsense is
// rejected at load time — most importantly a repo OUTSIDE the scope ceiling: a
// writable tree for a repo rein will never mint a credential for is incoherent
// (the agent could commit but never push), and it would widen the filesystem
// past the scope the human reviewed.
func TestWorktreesValidation(t *testing.T) {
	ok := Session{ID: "s", Repos: []string{"owner/a", "owner/b"},
		Worktrees: map[string]string{"owner/b": "/srv/dev/b"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid worktrees map rejected: %v", err)
	}

	for name, s := range map[string]Session{
		"out of scope": {ID: "s", Repos: []string{"owner/a"},
			Worktrees: map[string]string{"owner/z": "/srv/dev/z"}},
		"relative path": {ID: "s", Repos: []string{"owner/a"},
			Worktrees: map[string]string{"owner/a": "dev/a"}},
		"key not owner/name": {ID: "s", Repos: []string{"owner/a"},
			Worktrees: map[string]string{"justaname": "/srv/dev/a"}},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate accepted an invalid worktrees map", name)
		}
	}
}

// TestWorktreesRoundTripYAML: the field is hand-editable (mocks §4 — the yaml
// stays the standing ceiling) and survives a load.
func TestWorktreesRoundTripYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.yaml")
	body := "id: sess_x\nrole: implement\nrepos:\n  - owner/a\n  - owner/b\nworktrees:\n  owner/b: /srv/dev/b\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if s.Worktrees["owner/b"] != "/srv/dev/b" {
		t.Fatalf("worktrees did not round-trip: %v", s.Worktrees)
	}
}

func TestWarnIgnoredIssue(t *testing.T) {
	var buf strings.Builder
	s := Session{ID: "x", Role: "implement", Repos: []string{"o/a"}, Issue: 73}
	s.WarnIgnoredIssue(&buf)
	out := buf.String()
	if !strings.Contains(out, "IGNORED") || !strings.Contains(out, "rein declare") {
		t.Errorf("warning must be loud and name the declare command, got: %q", out)
	}
	buf.Reset()
	s.Issue = 0
	s.WarnIgnoredIssue(&buf)
	if buf.Len() != 0 {
		t.Errorf("no warning expected when issue is unset, got: %q", buf.String())
	}
}
