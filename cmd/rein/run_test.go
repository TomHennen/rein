package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRunArgs(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		wantErr bool
		wantCmd []string
	}{
		{"no args", nil, true, nil},
		{"just dashes", []string{"--"}, true, nil},
		{"no separator", []string{"claude"}, true, nil},
		{"separator + cmd", []string{"--", "claude"}, false, []string{"claude"}},
		{"separator + cmd + args", []string{"--", "bash", "-c", "echo hi"}, false, []string{"bash", "-c", "echo hi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRunArgs(tc.argv)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if len(got) != len(tc.wantCmd) {
					t.Fatalf("cmd = %v, want %v", got, tc.wantCmd)
				}
				for i := range got {
					if got[i] != tc.wantCmd[i] {
						t.Errorf("cmd[%d] = %q, want %q", i, got[i], tc.wantCmd[i])
					}
				}
			}
		})
	}
}

func TestWriteRunGitConfig_IncludesUserConfig(t *testing.T) {
	// Make a fake user gitconfig so the include.path line is emitted.
	home := t.TempDir()
	t.Setenv("HOME", home)
	userCfg := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(userCfg, []byte("[user]\n  name = test\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	path := filepath.Join(t.TempDir(), "out.gitconfig")
	if err := writeRunGitConfig(path, "/path/to/rein"); err != nil {
		t.Fatalf("writeRunGitConfig: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(body)
	checks := []string{
		"[include]",
		"path = " + userCfg,
		"[credential \"https://github.com\"]",
		"helper =",
		"/path/to/rein credential-helper",
		"useHttpPath = true",
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("gitconfig missing %q\n--- contents ---\n%s", c, s)
		}
	}
}

func TestWriteRunGitConfig_NoUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// No ~/.gitconfig file.
	path := filepath.Join(t.TempDir(), "out.gitconfig")
	if err := writeRunGitConfig(path, "/r"); err != nil {
		t.Fatalf("writeRunGitConfig: %v", err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "[include]") {
		t.Errorf("should not include user config when ~/.gitconfig is absent\n%s", string(body))
	}
	if !strings.Contains(string(body), "credential.https") && !strings.Contains(string(body), `[credential "https://github.com"]`) {
		t.Errorf("should still write credential helper config\n%s", string(body))
	}
}

func TestSetEnv(t *testing.T) {
	env := []string{"FOO=1", "BAR=2", "BAZ=3"}
	got := setEnv(env, "BAR", "new")
	wantHas := "BAR=new"
	wantNot := "BAR=2"
	if !contains(got, wantHas) {
		t.Errorf("missing %q in %v", wantHas, got)
	}
	if contains(got, wantNot) {
		t.Errorf("still has %q in %v", wantNot, got)
	}

	got = setEnv(env, "NEW", "appended")
	if !contains(got, "NEW=appended") {
		t.Errorf("missing appended NEW=appended in %v", got)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
