package prompt

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStubPrompter_MatchingResponseApproves(t *testing.T) {
	p := &StubPrompter{Response: "42"}
	res, err := p.Confirm(context.Background(), Request{Issue: 42})
	if err != nil {
		t.Fatalf("Confirm err: %v", err)
	}
	if !res.Approved {
		t.Error("expected approved=true for matching response")
	}
}

func TestStubPrompter_NonMatchingResponseDenies(t *testing.T) {
	p := &StubPrompter{Response: "99"}
	res, err := p.Confirm(context.Background(), Request{Issue: 42})
	if err != nil {
		t.Fatalf("Confirm err: %v", err)
	}
	if res.Approved {
		t.Error("expected approved=false for wrong response")
	}
}

func TestStubPrompter_TrimsWhitespace(t *testing.T) {
	p := &StubPrompter{Response: "  42  \n"}
	res, _ := p.Confirm(context.Background(), Request{Issue: 42})
	if !res.Approved {
		t.Error("expected approved=true for whitespace-padded response")
	}
}

func TestStubPrompter_ForceErr(t *testing.T) {
	want := errors.New("simulated tty failure")
	p := &StubPrompter{ForceErr: want}
	res, err := p.Confirm(context.Background(), Request{Issue: 1})
	if err != want {
		t.Errorf("err = %v, want %v", err, want)
	}
	if res.Approved {
		t.Error("approved must be false on forced error")
	}
}

func TestWritePrompt_FormA(t *testing.T) {
	var buf bytes.Buffer
	req := Request{
		SessionID: "sess_test_001",
		Role:      "implement",
		Repos:     []string{"Owner/Repo"},
		Issue:     73,
		IssueRepo: "Owner/Repo",
		Title:     "sbom-action v2 breaks when --json-output is set",
		State:     "open",
	}
	if err := writePrompt(&buf, req); err != nil {
		t.Fatalf("writePrompt: %v", err)
	}
	out := buf.String()
	// Key elements that must appear in the rendered Form A prompt (#35 §4):
	// the fetched TITLE + STATE + HOME REPO are the load-bearing
	// misattribution control (decision E).
	required := []string{
		"agent declares work on an issue",
		"#73",
		"sbom-action v2 breaks when --json-output is set",
		"[open]",
		"in Owner/Repo",
		"session:  sess_test_001",
		"role=implement",
		"covers ALL writes for this run",
		"type the issue number (73)",
	}
	for _, r := range required {
		if !strings.Contains(out, r) {
			t.Errorf("prompt missing %q\n\n%s", r, out)
		}
	}
}

func TestWritePrompt_ExpansionHeader(t *testing.T) {
	var buf bytes.Buffer
	if err := writePrompt(&buf, Request{Issue: 99, Expansion: true, Title: "t", State: "open"}); err != nil {
		t.Fatalf("writePrompt: %v", err)
	}
	if !strings.Contains(buf.String(), "ALSO work on") {
		t.Errorf("expansion prompt must carry the scope-expansion header, got:\n%s", buf.String())
	}
}

func TestWritePrompt_PRLabel(t *testing.T) {
	var buf bytes.Buffer
	if err := writePrompt(&buf, Request{Issue: 5, IsPR: true, Title: "a pr", State: "open"}); err != nil {
		t.Fatalf("writePrompt: %v", err)
	}
	if !strings.Contains(buf.String(), "[pull request]") {
		t.Errorf("PR declarations must be labeled (§9), got:\n%s", buf.String())
	}
}

func TestReadLineCtx_CancelUnblocks(t *testing.T) {
	// Reader that never returns — simulates a human who walked away.
	r := blockingReader{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := readLineCtx(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestReadLineCtx_DeliversInput(t *testing.T) {
	r := strings.NewReader("42\n")
	got, err := readLineCtx(context.Background(), r)
	if err != nil {
		t.Fatalf("readLineCtx err: %v", err)
	}
	if got != "42" {
		t.Errorf("got %q, want %q", got, "42")
	}
}

// blockingReader is an io.Reader that blocks indefinitely. Used to
// confirm readLineCtx honors context cancellation.
type blockingReader struct{}

func (blockingReader) Read(p []byte) (int, error) {
	select {} // block forever
}

// TestTTYPrompter_NoTTY exercises the no-controlling-terminal path.
// This test runs cleanly in CI / harness environments where /dev/tty
// is unavailable. Skipped if /dev/tty happens to be reachable (e.g.
// running locally in a real terminal).
func TestTTYPrompter_NoTTY(t *testing.T) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err == nil {
		f.Close()
		t.Skip("running in a context where /dev/tty IS reachable; nothing to assert")
	}
	p := TTYPrompter{}
	res, err := p.Confirm(context.Background(), Request{Issue: 1})
	if !errors.Is(err, ErrNoTTY) {
		t.Errorf("expected ErrNoTTY, got %v", err)
	}
	if res.Approved {
		t.Error("approved must be false when /dev/tty is unavailable")
	}
}
