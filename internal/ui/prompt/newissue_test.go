package prompt

import (
	"bytes"
	"strings"
	"testing"
)

// TestWritePrompt_NewIssue (issue #180): the human must see WHERE it
// would be filed, WHAT would be filed, and the exact word to type. The
// word displayed is the word compared — that is the property this pins.
func TestWritePrompt_NewIssue(t *testing.T) {
	var buf bytes.Buffer
	req := Request{
		SessionID: "s1", Role: "implement", Repos: []string{"o/r"},
		NewIssue: true, IssueRepo: "o/r",
		Title: "Add a thing", Body: "because reasons", ConfirmWord: "Add",
	}
	if err := writePrompt(&buf, req); err != nil {
		t.Fatalf("writePrompt: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"agent is requesting to file a new issue",
		"o/r",
		`"Add a thing"`,
		"because reasons",
		"To approve, type the proposed title's first word (Add)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("new-issue prompt missing %q:\n%s", want, got)
		}
	}
	// It is NOT the declared-issue ceremony: there is no number to type.
	if strings.Contains(got, "type the issue number") {
		t.Errorf("new-issue prompt must not ask for an issue number:\n%s", got)
	}
}

// TestWritePrompt_NewIssueDisclosures: two things the human cannot infer
// from the title and body alone — that rein adds an attribution line to
// what gets filed, and that this run already has issues (so a second
// filing is probably the agent losing the thread).
func TestWritePrompt_NewIssueDisclosures(t *testing.T) {
	var buf bytes.Buffer
	req := Request{
		NewIssue: true, IssueRepo: "o/r", Title: "Add a thing", ConfirmWord: "Add",
		Body: "because reasons", AlreadyConfirmed: []int{41, 42},
	}
	if err := writePrompt(&buf, req); err != nil {
		t.Fatalf("writePrompt: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"attribution line", "ALREADY confirmed", "#41, #42"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}

	// With nothing confirmed the note must be absent, not empty-rendered.
	buf.Reset()
	req.AlreadyConfirmed = nil
	if err := writePrompt(&buf, req); err != nil {
		t.Fatalf("writePrompt: %v", err)
	}
	if strings.Contains(buf.String(), "ALREADY confirmed") {
		t.Errorf("a first filing must not claim prior issues:\n%s", buf.String())
	}
}

func TestWritePrompt_NewIssueEmptyBody(t *testing.T) {
	var buf bytes.Buffer
	if err := writePrompt(&buf, Request{NewIssue: true, IssueRepo: "o/r", Title: "T", ConfirmWord: "T"}); err != nil {
		t.Fatalf("writePrompt: %v", err)
	}
	if !strings.Contains(buf.String(), "(none)") {
		t.Errorf("an empty body must render explicitly:\n%s", buf.String())
	}
}

// TestRequestMatches_ConfirmWord: the typed word is compared
// case-insensitively after trimming; anything else denies.
func TestRequestMatches_ConfirmWord(t *testing.T) {
	req := Request{NewIssue: true, ConfirmWord: "Añadir"}
	for _, ok := range []string{"Añadir", "añadir", "AÑADIR", "  añadir  "} {
		if !req.matches(ok) {
			t.Errorf("matches(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "Añadi", "soporte", "y", "0"} {
		if req.matches(bad) {
			t.Errorf("matches(%q) = true, want false", bad)
		}
	}
	// With no ConfirmWord the token is still the issue number, exactly.
	num := Request{Issue: 73}
	if !num.matches(" 73 ") || num.matches("Add") {
		t.Error("a declared-issue request must still compare the number")
	}
}

// TestRequestMatches_NewIssueWithoutConfirmWordDenies: Issue is 0 for a
// new-issue request, so a missing ConfirmWord must not fall through to
// "typing 0 approves". grantNewIssue builds its request from an on-disk
// snapshot, so the invariant is enforced across a file boundary and this
// is the guarantee at the gate.
func TestRequestMatches_NewIssueWithoutConfirmWordDenies(t *testing.T) {
	req := Request{NewIssue: true, Title: "whatever"}
	for _, answer := range []string{"0", "", "whatever", "yes"} {
		if req.matches(answer) {
			t.Errorf("matches(%q) = true on a new-issue request with no ConfirmWord", answer)
		}
	}
	if res, _ := (&StubPrompter{Response: "0"}).Confirm(t.Context(), req); res.Approved {
		t.Error("the stub must not approve a new-issue request with no ConfirmWord")
	}
}

// TestStubPrompter_NewIssueUsesConfirmWord keeps the test double honest:
// it must accept exactly what TTYPrompter would.
func TestStubPrompter_NewIssueUsesConfirmWord(t *testing.T) {
	req := Request{NewIssue: true, Title: "Add a thing", ConfirmWord: "Add"}
	if res, _ := (&StubPrompter{Response: "add"}).Confirm(t.Context(), req); !res.Approved {
		t.Error("the stub must approve on the first word, case-insensitively")
	}
	if res, _ := (&StubPrompter{Response: "thing"}).Confirm(t.Context(), req); res.Approved {
		t.Error("the stub approved a word that is not the first")
	}
}
