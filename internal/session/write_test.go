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

func TestAddExposePortToFile_CreatesAppendsAndRejects(t *testing.T) {
	p := writeSession(t, "id: sess_x\nrole: implement\nrepos:\n  - o/r   # keep me\n")
	u, err := AddExposePortToFile(p, 5173)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.ExposePorts) != 1 || u.ExposePorts[0] != 5173 {
		t.Errorf("after first add: %v", u.ExposePorts)
	}
	u, err = AddExposePortToFile(p, 8080)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.ExposePorts) != 2 || u.ExposePorts[1] != 8080 {
		t.Errorf("after second add: %v", u.ExposePorts)
	}
	if _, err := AddExposePortToFile(p, 5173); !errors.Is(err, ErrPortAlreadyExposed) {
		t.Errorf("duplicate: err = %v, want ErrPortAlreadyExposed", err)
	}
	for _, bad := range []int{0, 70000} {
		if _, err := AddExposePortToFile(p, bad); err == nil {
			t.Errorf("port %d accepted", bad)
		}
	}
	body, _ := os.ReadFile(p)
	if !strings.Contains(string(body), "# keep me") {
		t.Errorf("comment lost on node edit:\n%s", body)
	}
	if !strings.Contains(string(body), "expose_ports:") {
		t.Errorf("expose_ports key missing:\n%s", body)
	}
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

func TestAddAllowDomainToFile_CreatesAndAppends(t *testing.T) {
	// Key absent → created.
	p := writeSession(t, "# sess\nid: s\nrole: implement\nrepos:\n  - TomH/a\n")
	u, err := AddAllowDomainToFile(p, "pypi.org")
	if err != nil {
		t.Fatalf("add domain (create): %v", err)
	}
	if len(u.AllowDomains) != 1 || u.AllowDomains[0] != "pypi.org" {
		t.Fatalf("allow_domains = %v, want [pypi.org]", u.AllowDomains)
	}
	// Key present → appended; comments + repos survive.
	u, err = AddAllowDomainToFile(p, "*.golang.org")
	if err != nil {
		t.Fatalf("add domain (append): %v", err)
	}
	if len(u.AllowDomains) != 2 || u.AllowDomains[1] != "*.golang.org" {
		t.Fatalf("allow_domains = %v", u.AllowDomains)
	}
	body, _ := os.ReadFile(p)
	if !strings.Contains(string(body), "# sess") || !strings.Contains(string(body), "TomH/a") {
		t.Errorf("comments/repos must survive:\n%s", body)
	}
	// Duplicate (case-insensitive) → sentinel, nothing added.
	if _, err := AddAllowDomainToFile(p, "PyPI.org"); !errors.Is(err, ErrDomainAlreadyAllowed) {
		t.Errorf("duplicate must be ErrDomainAlreadyAllowed, got %v", err)
	}
}

func TestNormalizeAllowDomain_RejectsBadHosts(t *testing.T) {
	for _, ok := range []string{"pypi.org", "*.golang.org", "Api.GitHub.com", "files.pythonhosted.org."} {
		if _, err := NormalizeAllowDomain(ok); err != nil {
			t.Errorf("NormalizeAllowDomain(%q) rejected a valid host: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "*", "https://x.com", "x.com/path", "x.com:443", "has space.com", "nodot", "a.*.b", "foo.*"} {
		if _, err := NormalizeAllowDomain(bad); err == nil {
			t.Errorf("NormalizeAllowDomain(%q) accepted a bad host", bad)
		}
	}
}
