package appsetup

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestInstallOnRepoURL_SlugKnown(t *testing.T) {
	dir := t.TempDir()
	if err := WriteState(dir, State{
		Phase:   PhasePrimaryDone,
		Source:  SourceManifest,
		Primary: &AppRecord{Slug: "rein-primary-demo-abc123", HTMLURL: "https://github.com/apps/rein-primary-demo-abc123", CreatedAt: time.Now()},
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}
	url, haveSlug := InstallOnRepoURL(dir)
	if !haveSlug {
		t.Errorf("haveSlug = false, want true when a primary slug is recorded")
	}
	if url != "https://github.com/apps/rein-primary-demo-abc123/installations/new" {
		t.Errorf("url = %q", url)
	}
}

func TestInstallOnRepoURL_NoSlugFallsBackToGeneric(t *testing.T) {
	dir := t.TempDir() // no state.json at all
	url, haveSlug := InstallOnRepoURL(dir)
	if haveSlug {
		t.Errorf("haveSlug = true, want false when no slug is known")
	}
	if url != installGenericURL {
		t.Errorf("url = %q, want the generic installations URL", url)
	}
}

// OfferInstallOnRepo is print-only: it must emit a real link and the literal
// "visit this URL", and it must NEVER open a browser. We assert the second
// point structurally by installing a spy opener that fails the test if called.
func TestOfferInstallOnRepo_PrintsLinkNeverOpensBrowser(t *testing.T) {
	prev := browserOpenerOverride
	t.Cleanup(func() { browserOpenerOverride = prev })
	browserOpenerOverride = func(string) error {
		t.Fatalf("OfferInstallOnRepo must be print-only — it opened a browser")
		return nil
	}

	dir := t.TempDir()
	var buf bytes.Buffer
	OfferInstallOnRepo(&buf, dir, "octo/demo")
	out := buf.String()

	if !strings.Contains(out, "visit this URL") {
		t.Errorf("output missing the literal 'visit this URL':\n%s", out)
	}
	if !strings.Contains(out, installGenericURL) {
		t.Errorf("output missing a real link:\n%s", out)
	}
	if !strings.Contains(out, "octo/demo") {
		t.Errorf("output should name the session repo:\n%s", out)
	}
}
