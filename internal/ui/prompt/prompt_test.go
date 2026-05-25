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
	ok, err := p.Confirm(context.Background(), Request{Issue: 42})
	if err != nil {
		t.Fatalf("Confirm err: %v", err)
	}
	if !ok {
		t.Error("expected approved=true for matching response")
	}
}

func TestStubPrompter_NonMatchingResponseDenies(t *testing.T) {
	p := &StubPrompter{Response: "99"}
	ok, err := p.Confirm(context.Background(), Request{Issue: 42})
	if err != nil {
		t.Fatalf("Confirm err: %v", err)
	}
	if ok {
		t.Error("expected approved=false for wrong response")
	}
}

func TestStubPrompter_TrimsWhitespace(t *testing.T) {
	p := &StubPrompter{Response: "  42  \n"}
	ok, _ := p.Confirm(context.Background(), Request{Issue: 42})
	if !ok {
		t.Error("expected approved=true for whitespace-padded response")
	}
}

func TestStubPrompter_ForceErr(t *testing.T) {
	want := errors.New("simulated tty failure")
	p := &StubPrompter{ForceErr: want}
	ok, err := p.Confirm(context.Background(), Request{Issue: 1})
	if err != want {
		t.Errorf("err = %v, want %v", err, want)
	}
	if ok {
		t.Error("approved must be false on forced error")
	}
}

func TestWritePrompt_Format(t *testing.T) {
	var buf bytes.Buffer
	req := Request{
		SessionID: "sess_test_001",
		Role:      "implement",
		Repo:      "Owner/Repo",
		Action:    "git push",
		Issue:     73,
	}
	if err := writePrompt(&buf, req); err != nil {
		t.Fatalf("writePrompt: %v", err)
	}
	out := buf.String()
	// Key elements that must appear in the rendered prompt.
	required := []string{
		"rein: write access requested",
		"action:  git push",
		"repo:    Owner/Repo",
		"session: sess_test_001",
		"role=implement",
		"issue=#73",
		"type the issue number (73)",
	}
	for _, r := range required {
		if !strings.Contains(out, r) {
			t.Errorf("prompt missing %q\n\n%s", r, out)
		}
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
	ok, err := p.Confirm(context.Background(), Request{Issue: 1})
	if !errors.Is(err, ErrNoTTY) {
		t.Errorf("expected ErrNoTTY, got %v", err)
	}
	if ok {
		t.Error("approved must be false when /dev/tty is unavailable")
	}
}
