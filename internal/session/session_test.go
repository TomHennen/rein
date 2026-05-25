package session

import (
	"errors"
	"os"
	"path/filepath"
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
