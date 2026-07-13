package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/gitidentity"
)

// The managed sandbox gitconfig must set core.excludesFile at a rein-written
// ignore list that hides srt's injected agent-env dotfiles (#102) — so they
// don't read as untracked changes or get swept into `git add -A` — WITHOUT
// hiding files an agent legitimately creates (e.g. .gitignore).
func TestManagedGitConfigExcludesSrtDotfiles(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gitconfig")
	if err := writeManagedGitConfig(cfg, gitidentity.Identity{Name: "bot", Email: "bot@example.invalid"}); err != nil {
		t.Fatalf("writeManagedGitConfig: %v", err)
	}

	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "excludesFile = ") {
		t.Fatalf("managed config missing core.excludesFile:\n%s", body)
	}

	eb, err := os.ReadFile(filepath.Join(dir, "rein-sandbox.gitignore"))
	if err != nil {
		t.Fatalf("excludes file not written: %v", err)
	}
	got := string(eb)
	for _, f := range srtInjectedDotfiles {
		// root-anchored (leading slash) so a same-named file deeper in the tree
		// is NOT hidden — only srt's root injection.
		want := "/" + f + "\n"
		if !strings.Contains(got, want) {
			t.Errorf("excludes missing root-anchored %q:\n%s", want, got)
		}
	}
	// Files an agent commonly creates and DOES want to commit must not be hidden.
	for _, keep := range []string{".gitignore", ".gitattributes", ".dockerignore", ".editorconfig"} {
		if strings.Contains(got, "/"+keep+"\n") {
			t.Errorf("must not exclude %q (agents legitimately create it)", keep)
		}
	}
}
