package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSession(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "dev-session.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAddRepoToFile_AppendsSameOwner(t *testing.T) {
	p := writeSession(t, "id: s\nrole: implement\nrepos:\n  - TomH/a\n")
	updated, err := AddRepoToFile(p, "TomH/b")
	if err != nil {
		t.Fatalf("add same-owner repo: %v", err)
	}
	if !updated.Contains("TomH/b") {
		t.Error("returned session must contain the added repo")
	}
	// The file on disk must reload and contain both repos.
	reloaded, err := LoadFromFile(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Repos) != 2 || !reloaded.Contains("TomH/a") || !reloaded.Contains("TomH/b") {
		t.Fatalf("file must hold both repos, got %v", reloaded.Repos)
	}
}

func TestAddRepoToFile_PreservesComments(t *testing.T) {
	p := writeSession(t, "# my session\nid: s  # forensic id\nrole: implement\nrepos:\n  - TomH/a\nallow_domains:\n  - registry.npmjs.org\n")
	if _, err := AddRepoToFile(p, "TomH/b"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(p)
	s := string(body)
	if !strings.Contains(s, "# my session") || !strings.Contains(s, "# forensic id") {
		t.Errorf("comments must survive the edit:\n%s", s)
	}
	if !strings.Contains(s, "registry.npmjs.org") {
		t.Errorf("other keys must survive the edit:\n%s", s)
	}
}

func TestAddRepoToFile_CrossOwnerRejected(t *testing.T) {
	p := writeSession(t, "id: s\nrole: implement\nrepos:\n  - TomH/a\n")
	_, err := AddRepoToFile(p, "someone-else/b")
	if err == nil || !strings.Contains(err.Error(), "single-owner") {
		t.Fatalf("cross-owner add must be rejected with the single-owner reason, got %v", err)
	}
	// Nothing written.
	reloaded, _ := LoadFromFile(p)
	if len(reloaded.Repos) != 1 {
		t.Errorf("nothing may be written on a rejected add, got %v", reloaded.Repos)
	}
}

func TestAddRepoToFile_DuplicateIsSentinel(t *testing.T) {
	p := writeSession(t, "id: s\nrole: implement\nrepos:\n  - TomH/a\n")
	if _, err := AddRepoToFile(p, "TomH/a"); !errors.Is(err, ErrRepoAlreadyInSession) {
		t.Fatalf("duplicate add must return ErrRepoAlreadyInSession, got %v", err)
	}
	// A trailing .git / case variation is still a duplicate.
	if _, err := AddRepoToFile(p, "tomh/a.git"); !errors.Is(err, ErrRepoAlreadyInSession) {
		t.Fatalf("normalized duplicate must be a sentinel, got %v", err)
	}
}

func TestCheckAddRepo_ShapeAndSuggestion(t *testing.T) {
	s := Session{ID: "s", Repos: []string{"TomH/a"}}
	_, err := CheckAddRepo(s, "justaname")
	if err == nil || !strings.Contains(err.Error(), "TomH/justaname") {
		t.Fatalf("a bare name must be rejected with an owner/name suggestion, got %v", err)
	}
}

func TestOwnerOf_PreservesCase(t *testing.T) {
	s := Session{ID: "s", Repos: []string{"TomHennen/a"}}
	if got := OwnerOf(s); got != "TomHennen" {
		t.Errorf("OwnerOf must preserve case, got %q", got)
	}
}

func TestSourceFilePath(t *testing.T) {
	if got := SourceFilePath("file:/x/y.yaml"); got != "/x/y.yaml" {
		t.Errorf("file: source => path, got %q", got)
	}
	if got := SourceFilePath("env-fallback"); got != "" {
		t.Errorf("non-file source => empty, got %q", got)
	}
}
