package runscope

import (
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/session"
)

func sess() session.Session {
	return session.Session{ID: "s", Role: "implement", Repos: []string{"TomH/a"}}
}

// record appends a confirmed issue for repo/number to the run, valid under
// sess's signature.
func record(t *testing.T, stateDir, runID string, s session.Session, repo string, number int) {
	t.Helper()
	sig := approvals.SignatureOf(s)
	ci := approvals.ConfirmedIssue{Number: number, Repo: repo, Title: "t", State: "open"}
	if err := approvals.AppendConfirmedIssue(stateDir, runID, sig, s.ID, ci, time.Hour); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestResolver_NoExpansionsIsStandingScope(t *testing.T) {
	r := New(sess(), t.TempDir(), "run")
	if got := r.Repos(); len(got) != 1 || got[0] != "TomH/a" {
		t.Fatalf("no expansions => standing repos, got %v", got)
	}
	if r.Contains("TomH/b") {
		t.Error("repo b must not be in scope with no expansion")
	}
}

func TestResolver_ApprovedExpansionWidens(t *testing.T) {
	s := sess()
	dir := t.TempDir()
	// A same-repo issue (in the session) is NOT an expansion.
	record(t, dir, "run", s, "TomH/a", 1)
	// A different repo (same owner) IS an expansion.
	record(t, dir, "run", s, "TomH/b", 2)
	r := New(s, dir, "run")

	if !r.Contains("TomH/b") {
		t.Error("approved expansion repo must be in scope")
	}
	if exp := r.Expansions(); len(exp) != 1 || exp[0] != "TomH/b" {
		t.Fatalf("expansions must be exactly [TomH/b], got %v", exp)
	}
	// BareNames feeds the mint scope: it must include both names.
	names := r.BareNames()
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("BareNames must cover the union, got %v", names)
	}
}

func TestResolver_CrossOwnerExpansionDropped(t *testing.T) {
	s := sess()
	dir := t.TempDir()
	// A malformed/hand-injected cross-owner confirmed issue must NOT widen
	// scope (defense in depth — declare rejects these before recording).
	record(t, dir, "run", s, "evil/b", 3)
	r := New(s, dir, "run")
	if r.Contains("evil/b") {
		t.Error("a cross-owner recorded repo must be dropped from the ceiling")
	}
	if len(r.Expansions()) != 0 {
		t.Errorf("cross-owner must not appear as an expansion, got %v", r.Expansions())
	}
	for _, n := range r.BareNames() {
		if n == "b" {
			t.Error("cross-owner bare name must not reach the mint scope")
		}
	}
}

func TestResolver_KeyChangesWithCeiling(t *testing.T) {
	s := sess()
	dir := t.TempDir()
	before := New(s, dir, "run").Key()
	record(t, dir, "run", s, "TomH/b", 2)
	after := New(s, dir, "run").Key()
	if before == after {
		t.Error("Key must change when the ceiling grows (drives cache busting)")
	}
	// Key is case-insensitive over the standing repos (a re-cased yaml must
	// not cause a spurious re-mint). Use an empty run so only the standing
	// repos contribute.
	empty := t.TempDir()
	k1 := New(sess(), empty, "run").Key()
	k2 := New(session.Session{ID: "s", Role: "implement", Repos: []string{"tomh/A"}}, empty, "run").Key()
	if k1 != k2 {
		t.Errorf("Key must be case-insensitive: %q vs %q", k1, k2)
	}
}

func TestResolver_SignatureMismatchCollapsesScope(t *testing.T) {
	s := sess()
	dir := t.TempDir()
	record(t, dir, "run", s, "TomH/b", 2)
	// A DIFFERENT session (e.g. a mid-run yaml edit) has a different
	// signature; the record no longer validates, so no expansion is served.
	edited := session.Session{ID: "s", Role: "implement", Repos: []string{"TomH/a", "TomH/c"}}
	r := New(edited, dir, "run")
	if r.Contains("TomH/b") {
		t.Error("a signature mismatch must collapse the ceiling to the standing repos (fail closed)")
	}
}
