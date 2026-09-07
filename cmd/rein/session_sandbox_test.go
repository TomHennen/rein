package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/srt"
)

// TestPrintSandboxSessionView (issue #180): in-sandbox `rein session
// show` used to report "no session file", which the agent read as
// breakage. The replacement must state the run's actual scope and both
// declare forms — and must not invite the agent to fix scope itself.
func TestPrintSandboxSessionView(t *testing.T) {
	t.Setenv(srt.EnvInSandboxRepos, "o/r,o/other")
	var buf bytes.Buffer
	printSandboxSessionView(&buf)
	got := buf.String()
	for _, want := range []string{
		"inside the rein sandbox",
		"fixed at launch",
		"o/r, o/other",
		"rein declare <n>",
		`rein declare --new "<title>"`,
		"only be changed by the human",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("in-sandbox view missing %q:\n%s", want, got)
		}
	}
	// The old error text is what made this read as breakage.
	if strings.Contains(got, "no session file at") {
		t.Errorf("the in-sandbox view must not report a missing file as a fault:\n%s", got)
	}
}

// TestPrintSandboxSessionView_NoReposReported: a blank scope must be
// visibly unknown, never rendered as an empty repo list.
func TestPrintSandboxSessionView_NoReposReported(t *testing.T) {
	t.Setenv(srt.EnvInSandboxRepos, "")
	var buf bytes.Buffer
	printSandboxSessionView(&buf)
	if !strings.Contains(buf.String(), "(not reported)") {
		t.Errorf("missing scope must be explicit:\n%s", buf.String())
	}
}
