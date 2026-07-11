package approvals

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/session"
)

func TestSignatureOf_Stable(t *testing.T) {
	s := session.Session{
		ID:    "sess_1",
		Role:  "implement",
		Repos: []string{"o/a", "o/b"},
		Issue: 7,
	}
	a := SignatureOf(s)
	b := SignatureOf(s)
	if a != b {
		t.Errorf("signature not stable: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Errorf("expected sha256 hex (64 chars), got len=%d", len(a))
	}
}

func TestSignatureOf_ChangesPerField(t *testing.T) {
	base := session.Session{ID: "x", Role: "implement", Repos: []string{"o/a"}, Issue: 1}
	cases := []struct {
		name   string
		mutate func(s *session.Session)
	}{
		{"id changes", func(s *session.Session) { s.ID = "y" }},
		{"role changes", func(s *session.Session) { s.Role = "scan" }},
		{"repos add", func(s *session.Session) { s.Repos = []string{"o/a", "o/b"} }},
		{"repos different", func(s *session.Session) { s.Repos = []string{"o/c"} }},
	}
	baseSig := SignatureOf(base)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mutate(&s)
			if SignatureOf(s) == baseSig {
				t.Errorf("expected signature change, got identical")
			}
		})
	}
}

// TestSignatureOf_IssueIgnored pins issue #35's decision A in data form:
// the legacy `issue:` field moved from approval IDENTITY to approval
// CONTENT (Record.Issues), so two sessions differing only in Issue have
// the SAME signature — a leftover `issue:` in a session file can neither
// create nor invalidate an approval.
func TestSignatureOf_IssueIgnored(t *testing.T) {
	a := SignatureOf(session.Session{ID: "x", Role: "r", Repos: []string{"o/a"}, Issue: 1})
	b := SignatureOf(session.Session{ID: "x", Role: "r", Repos: []string{"o/a"}, Issue: 2})
	if a != b {
		t.Errorf("Issue must not affect signature (issue #35 decision A): %q vs %q", a, b)
	}
}

func TestSignatureOf_RepoOrderInsensitive(t *testing.T) {
	a := SignatureOf(session.Session{ID: "x", Role: "r", Repos: []string{"o/a", "o/b"}, Issue: 1})
	b := SignatureOf(session.Session{ID: "x", Role: "r", Repos: []string{"o/b", "o/a"}, Issue: 1})
	if a != b {
		t.Errorf("repo order should not affect signature: %q vs %q", a, b)
	}
}

func TestSignatureOf_CreatedIgnored(t *testing.T) {
	a := SignatureOf(session.Session{ID: "x", Role: "r", Repos: []string{"o/a"}, Issue: 1, Created: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)})
	b := SignatureOf(session.Session{ID: "x", Role: "r", Repos: []string{"o/a"}, Issue: 1, Created: time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)})
	if a != b {
		t.Errorf("Created should not affect signature: %q vs %q", a, b)
	}
}

// TestValid_NoTimeGate proves the time gate was removed: a record whose
// ExpiresAt is in the past is STILL valid when signatures match. The run
// lifetime — not a clock — is the bound now.
func TestValid_NoTimeGate(t *testing.T) {
	past := Record{
		Signature:  "abc",
		ApprovedAt: time.Now().Add(-48 * time.Hour),
		ExpiresAt:  time.Now().Add(-24 * time.Hour), // long expired
	}
	if !Valid(past, "abc") {
		t.Error("expired-by-clock record must still be Valid when signature matches (no time gate)")
	}
	if Valid(past, "xyz") {
		t.Error("mismatched signature must be invalid")
	}
	if Valid(Record{}, "abc") {
		t.Error("empty signature must be invalid")
	}
	if Valid(past, "") {
		t.Error("empty expected must be invalid")
	}
}

func TestRunPaths_DistinctAndSanitized(t *testing.T) {
	dir := "/state"
	ap := RunApprovalPath(dir, "RUN123")
	rc := RunContextPath(dir, "RUN123")
	if ap == rc {
		t.Errorf("approval and context paths must differ: %q", ap)
	}
	if filepath.Dir(ap) != filepath.Join(dir, "approvals") {
		t.Errorf("approval path dir = %q", filepath.Dir(ap))
	}
	if filepath.Dir(rc) != filepath.Join(dir, "runs") {
		t.Errorf("context path dir = %q", filepath.Dir(rc))
	}
	// Path-traversal in a malformed --run-id must be neutralized.
	evil := RunApprovalPath(dir, "../../etc/passwd")
	if filepath.Dir(evil) != filepath.Join(dir, "approvals") {
		t.Errorf("traversal not contained: %q", evil)
	}
}

func TestApproval_WriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Record{
		Signature:  "abc123",
		SessionID:  "sess_x",
		ApprovedAt: time.Now().Truncate(time.Second).UTC(),
		ExpiresAt:  time.Now().Add(4 * time.Hour).Truncate(time.Second).UTC(),
	}
	if err := WriteApproval(dir, "run_a", want); err != nil {
		t.Fatalf("WriteApproval: %v", err)
	}
	info, err := os.Stat(RunApprovalPath(dir, "run_a"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
	got, err := ReadApproval(dir, "run_a")
	if err != nil {
		t.Fatalf("ReadApproval: %v", err)
	}
	if got.Signature != want.Signature || got.SessionID != want.SessionID || !got.ApprovedAt.Equal(want.ApprovedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, want)
	}
}

func TestRunContext_WriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := RunContext{
		Session:   session.Session{ID: "sess_y", Role: "implement", Repos: []string{"o/a"}, Issue: 7},
		RunPID:    4242,
		WrittenAt: time.Now().Truncate(time.Second).UTC(),
	}
	if err := WriteRunContext(dir, "run_b", want); err != nil {
		t.Fatalf("WriteRunContext: %v", err)
	}
	got, err := ReadRunContext(dir, "run_b")
	if err != nil {
		t.Fatalf("ReadRunContext: %v", err)
	}
	if got.Session.ID != want.Session.ID || got.RunPID != want.RunPID || got.Session.Issue != want.Session.Issue {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, want)
	}
}

func TestReadApproval_MissingIsErrNotExist(t *testing.T) {
	if _, err := ReadApproval(t.TempDir(), "absent"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
	if _, err := ReadRunContext(t.TempDir(), "absent"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist for context, got %v", err)
	}
}

func TestClearRun_RemovesBothAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := WriteApproval(dir, "r", Record{Signature: "x"}); err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	if err := WriteRunContext(dir, "r", RunContext{RunPID: 1}); err != nil {
		t.Fatalf("seed context: %v", err)
	}
	if err := ClearRun(dir, "r"); err != nil {
		t.Fatalf("ClearRun: %v", err)
	}
	if _, err := os.Stat(RunApprovalPath(dir, "r")); !errors.Is(err, os.ErrNotExist) {
		t.Error("approval file should be gone")
	}
	if _, err := os.Stat(RunContextPath(dir, "r")); !errors.Is(err, os.ErrNotExist) {
		t.Error("context file should be gone")
	}
	// Idempotent: clearing again is not an error.
	if err := ClearRun(dir, "r"); err != nil {
		t.Errorf("second ClearRun should be a no-op, got %v", err)
	}
}

func TestSweep_LivePidSurvives(t *testing.T) {
	dir := t.TempDir()
	// Our own pid is alive; an ancient WrittenAt must NOT cause a sweep.
	rc := RunContext{RunPID: os.Getpid(), WrittenAt: time.Now().Add(-72 * time.Hour)}
	if err := WriteRunContext(dir, "live", rc); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := WriteApproval(dir, "live", Record{Signature: "s"}); err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	if err := Sweep(dir, 24*time.Hour, time.Now()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(RunApprovalPath(dir, "live")); err != nil {
		t.Errorf("live run's approval should survive sweep regardless of age: %v", err)
	}
}

func TestSweep_DeadPidRemoved(t *testing.T) {
	dir := t.TempDir()
	dead := findDeadPID(t)
	rc := RunContext{RunPID: dead, WrittenAt: time.Now()} // recent, but dead owner
	if err := WriteRunContext(dir, "dead", rc); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := WriteApproval(dir, "dead", Record{Signature: "s"}); err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	if err := Sweep(dir, 24*time.Hour, time.Now()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(RunApprovalPath(dir, "dead")); !errors.Is(err, os.ErrNotExist) {
		t.Error("dead run's approval should be swept")
	}
	if _, err := os.Stat(RunContextPath(dir, "dead")); !errors.Is(err, os.ErrNotExist) {
		t.Error("dead run's context should be swept")
	}
}

func TestSweep_UnknownPidOldRemoved(t *testing.T) {
	dir := t.TempDir()
	// RunPID 0 (unknown) + old WrittenAt -> backstop age sweep.
	rc := RunContext{RunPID: 0, WrittenAt: time.Now().Add(-48 * time.Hour)}
	if err := WriteRunContext(dir, "old", rc); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Sweep(dir, 24*time.Hour, time.Now()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(RunContextPath(dir, "old")); !errors.Is(err, os.ErrNotExist) {
		t.Error("old unknown-pid context should be swept by age backstop")
	}
}

func TestSweep_UnknownPidRecentSurvives(t *testing.T) {
	dir := t.TempDir()
	rc := RunContext{RunPID: 0, WrittenAt: time.Now()} // unknown pid but recent
	if err := WriteRunContext(dir, "fresh", rc); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Sweep(dir, 24*time.Hour, time.Now()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(RunContextPath(dir, "fresh")); err != nil {
		t.Errorf("recent unknown-pid context should survive: %v", err)
	}
}

// TestSweep_IgnoresLegacyGlobalApprovalJSON guards the pre-upgrade global
// approval.json migration. Sweep scans only the runs/ and approvals/
// subdirs, so a leftover top-level approval.json (the old single-file shape)
// must be ignored — not crash the sweep. The actual REMOVAL of that legacy
// file happens at launch in runWrapped (cmd/rein/run.go: os.Remove of
// stateDir/approval.json), which is intentionally separate from Sweep; this
// test pins Sweep's "leave it untouched, sweep the orphan" behavior.
func TestSweep_IgnoresLegacyGlobalApprovalJSON(t *testing.T) {
	dir := t.TempDir()

	// Seed a legacy global approval.json in the old single-file shape, at
	// the top level of the state dir (not under runs/ or approvals/).
	legacy := filepath.Join(dir, "approval.json")
	if err := os.WriteFile(legacy, []byte(`{"signature":"old","session_id":"legacy"}`), 0o600); err != nil {
		t.Fatalf("seed legacy approval.json: %v", err)
	}

	// Also seed an orphan per-run file (dead owner) so we confirm Sweep
	// still does its real job alongside the legacy file.
	dead := findDeadPID(t)
	if err := WriteRunContext(dir, "orphan", RunContext{RunPID: dead, WrittenAt: time.Now()}); err != nil {
		t.Fatalf("seed orphan context: %v", err)
	}
	if err := WriteApproval(dir, "orphan", Record{Signature: "s"}); err != nil {
		t.Fatalf("seed orphan approval: %v", err)
	}

	if err := Sweep(dir, 24*time.Hour, time.Now()); err != nil {
		t.Fatalf("Sweep must not error on a state dir with a legacy approval.json: %v", err)
	}

	// Sweep ignores the legacy top-level file: it is untouched (removal is
	// run.go's launch migration, not Sweep's job).
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("legacy approval.json should be left untouched by Sweep, got: %v", err)
	}

	// The orphan per-run files were swept as usual.
	if _, err := os.Stat(RunContextPath(dir, "orphan")); !errors.Is(err, os.ErrNotExist) {
		t.Error("orphan context should be swept")
	}
	if _, err := os.Stat(RunApprovalPath(dir, "orphan")); !errors.Is(err, os.ErrNotExist) {
		t.Error("orphan approval should be swept")
	}
}

func TestList_ReportsHasAndLiveFlags(t *testing.T) {
	dir := t.TempDir()
	// Run A: context (live) + approval.
	if err := WriteRunContext(dir, "A", RunContext{RunPID: os.Getpid(), WrittenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := WriteApproval(dir, "A", Record{Signature: "sa"}); err != nil {
		t.Fatal(err)
	}
	// Run B: approval only (no context).
	if err := WriteApproval(dir, "B", Record{Signature: "sb"}); err != nil {
		t.Fatal(err)
	}
	list, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 runs, got %d: %+v", len(list), list)
	}
	byID := map[string]RunStatus{}
	for _, s := range list {
		byID[s.RunID] = s
	}
	a := byID["A"]
	if !a.HasContext || !a.HasApproval || !a.Live {
		t.Errorf("run A flags wrong: %+v", a)
	}
	b := byID["B"]
	if b.HasContext || !b.HasApproval || b.Live {
		t.Errorf("run B flags wrong: %+v", b)
	}
}

func TestList_EmptyStateDir(t *testing.T) {
	list, err := List(t.TempDir())
	if err != nil {
		t.Fatalf("List on empty dir: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d", len(list))
	}
}

// findDeadPID returns a pid that is not currently alive. We scan upward
// from a high value to find an unused pid.
func findDeadPID(t *testing.T) int {
	t.Helper()
	for pid := 4000000; pid < 4000100; pid++ {
		if !pidAlive(pid) {
			return pid
		}
	}
	t.Skip("could not find a dead pid to test with")
	return 0
}

// ---- issue #35: confirmed-issue set ----

func ci(repo string, n int) ConfirmedIssue {
	return ConfirmedIssue{
		Number:       n,
		Repo:         repo,
		Title:        "a title",
		State:        "open",
		CanonicalURL: "https://api.github.com/repos/" + repo + "/issues/" + fmt.Sprint(n),
		ConfirmedAt:  time.Now(),
	}
}

func TestRecord_HasIssue(t *testing.T) {
	r := Record{Issues: []ConfirmedIssue{ci("o/a", 73)}}
	if !r.HasIssue("o/a", 73) {
		t.Error("HasIssue should find the confirmed issue")
	}
	if !r.HasIssue("O/A", 73) {
		t.Error("HasIssue must compare repo case-insensitively (GitHub path semantics)")
	}
	if r.HasIssue("o/a", 74) {
		t.Error("HasIssue must not match a different number")
	}
	if r.HasIssue("o/b", 73) {
		t.Error("HasIssue must not match the same number in a different repo (S4)")
	}
}

func TestAppendConfirmedIssue_FreshAppendIdempotent(t *testing.T) {
	dir := t.TempDir()
	sig := "sig1"

	// Fresh: no record exists yet.
	if err := AppendConfirmedIssue(dir, "run1", sig, "sess", ci("o/a", 73), time.Hour); err != nil {
		t.Fatalf("append (fresh): %v", err)
	}
	rec, err := ReadApproval(dir, "run1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if rec.Signature != sig || rec.SessionID != "sess" || len(rec.Issues) != 1 {
		t.Fatalf("unexpected fresh record: %+v", rec)
	}
	if rec.ApprovedAt.IsZero() || rec.ExpiresAt.IsZero() {
		t.Error("fresh record should stamp ApprovedAt/ExpiresAt")
	}

	// Append a second issue (the expansion path).
	if err := AppendConfirmedIssue(dir, "run1", sig, "sess", ci("o/a", 99), time.Hour); err != nil {
		t.Fatalf("append (expand): %v", err)
	}
	rec, _ = ReadApproval(dir, "run1")
	if len(rec.Issues) != 2 || !rec.HasIssue("o/a", 73) || !rec.HasIssue("o/a", 99) {
		t.Fatalf("expected both issues in the set, got %+v", rec.Issues)
	}

	// Idempotent: re-confirming an issue already in the set adds nothing.
	if err := AppendConfirmedIssue(dir, "run1", sig, "sess", ci("o/a", 73), time.Hour); err != nil {
		t.Fatalf("append (idempotent): %v", err)
	}
	rec, _ = ReadApproval(dir, "run1")
	if len(rec.Issues) != 2 {
		t.Fatalf("idempotent re-confirm must not grow the set: %+v", rec.Issues)
	}
}

func TestAppendConfirmedIssue_SignatureMismatchResetsSet(t *testing.T) {
	dir := t.TempDir()
	if err := AppendConfirmedIssue(dir, "run1", "oldsig", "sess", ci("o/a", 73), time.Hour); err != nil {
		t.Fatal(err)
	}
	// A mid-run session edit changed the signature: the whole record —
	// including its issue set — is invalid. The next confirm starts fresh.
	if err := AppendConfirmedIssue(dir, "run1", "newsig", "sess", ci("o/a", 99), time.Hour); err != nil {
		t.Fatal(err)
	}
	rec, _ := ReadApproval(dir, "run1")
	if rec.Signature != "newsig" {
		t.Errorf("record should carry the new signature, got %q", rec.Signature)
	}
	if rec.HasIssue("o/a", 73) {
		t.Error("stale issue confirmed under the old signature must NOT survive (mid-run edit invalidates the whole record)")
	}
	if !rec.HasIssue("o/a", 99) {
		t.Error("fresh issue must be present")
	}
}

func TestConfirmedIssues_FailClosedPaths(t *testing.T) {
	dir := t.TempDir()

	if got := ConfirmedIssues(dir, "", "sig"); got != nil {
		t.Error("empty run id must yield nil (outside rein run — fail closed)")
	}
	if got := ConfirmedIssues(dir, "run1", "sig"); got != nil {
		t.Error("missing record must yield nil")
	}
	if err := AppendConfirmedIssue(dir, "run1", "sig", "sess", ci("o/a", 73), time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := ConfirmedIssues(dir, "run1", "othersig"); got != nil {
		t.Error("signature mismatch must yield nil (mid-run edit fail-closed)")
	}
	got := ConfirmedIssues(dir, "run1", "sig")
	if len(got) != 1 || got[0].Number != 73 {
		t.Errorf("expected the confirmed set, got %+v", got)
	}
}

func TestRunContext_PendingIssueRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pi := ci("o/a", 73)
	rc := RunContext{
		Session:      session.Session{ID: "s", Role: "implement", Repos: []string{"o/a"}},
		RunPID:       123,
		PendingIssue: &pi,
		WrittenAt:    time.Now(),
	}
	if err := WriteRunContext(dir, "run1", rc); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRunContext(dir, "run1")
	if err != nil {
		t.Fatal(err)
	}
	if got.PendingIssue == nil || got.PendingIssue.Number != 73 || got.PendingIssue.Title != "a title" {
		t.Errorf("PendingIssue did not round-trip: %+v", got.PendingIssue)
	}
}

// TestRecord_HasIssue_NormalizesRepo is the HIGH-1 regression (security
// review round 2): the proxy derives the push-target repo from the git
// smart-HTTP URL, so `/o/r.git/git-receive-pack` yields "o/r.git", while a
// declare records the bare "o/r" it resolved from the session. A raw
// compare denied a correctly declared+confirmed push on GitHub's DEFAULT
// clone URL shape. The comparator must normalize both sides.
func TestRecord_HasIssue_NormalizesRepo(t *testing.T) {
	// Recorded bare (what declare stores); queried with .git (what the
	// proxy derives from the real remote URL).
	bare := Record{Issues: []ConfirmedIssue{ci("o/r", 73)}}
	if !bare.HasIssue("o/r.git", 73) {
		t.Error("a push to https://github.com/o/r.git must match the confirmed bare o/r (HIGH-1)")
	}
	// And the reverse (a session/declare that recorded a .git-suffixed repo).
	dotgit := Record{Issues: []ConfirmedIssue{ci("o/r.git", 73)}}
	if !dotgit.HasIssue("o/r", 73) {
		t.Error("normalization must be symmetric")
	}
	// Case + trailing slash + leading slash, per brokercore.RepoFromPath.
	for _, q := range []string{"O/R.GIT", "o/r/", "/o/r", "/o/r.git/"} {
		if !bare.HasIssue(q, 73) {
			t.Errorf("query %q should normalize to o/r and match", q)
		}
	}
	// Still fails closed on genuinely different repos / numbers / garbage.
	for _, q := range []string{"o/other", "other/r", "notarepo", ""} {
		if bare.HasIssue(q, 73) {
			t.Errorf("query %q must NOT match o/r (fail closed)", q)
		}
	}
	if bare.HasIssue("o/r.git", 74) {
		t.Error("a different issue number must never match")
	}
}
