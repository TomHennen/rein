package main

import (
	"os"
	"strings"
	"testing"
)

// nonTTY returns a *os.File that is NOT a terminal (the read end of a pipe),
// so resolveMachineLabel takes the non-interactive fallback path without
// blocking on a prompt. The genuinely-interactive prompt (a human editing the
// pre-filled hostname at /dev/tty) is covered by the pexpect journey/test —
// a unit test cannot honestly stand in for a real tty.
func nonTTY(t *testing.T) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	return r
}

func TestResolveMachineLabel_FlagWins(t *testing.T) {
	// --machine-label overrides everything, and is sanitized on the way in.
	t.Setenv("REIN_MACHINE_HOSTNAME", "some-host")
	if got := resolveMachineLabel("My Box!", nonTTY(t), false); got != "my-box" {
		t.Errorf("flag label = %q, want my-box", got)
	}
}

func TestResolveMachineLabel_NonInteractiveUsesHostname(t *testing.T) {
	// No flag, no tty (or --yes): fall back to the sanitized hostname without
	// prompting — never block (guardrail §7). Fixture kept within the label
	// budget so it isn't truncated (the cap is exercised separately below).
	t.Setenv("REIN_MACHINE_HOSTNAME", "Toms-Box")
	if got := resolveMachineLabel("", nonTTY(t), false); got != "toms-box" {
		t.Errorf("hostname fallback = %q, want toms-box", got)
	}
	// --yes also forces the non-interactive fallback even on a tty.
	if got := resolveMachineLabel("", nonTTY(t), true); got != "toms-box" {
		t.Errorf("--yes fallback = %q, want toms-box", got)
	}
}

func TestResolveMachineLabel_LongHostnameTruncated(t *testing.T) {
	// A long distinctive hostname is sanitized AND capped, so the eventual App
	// name stays within GitHub's limit (the cap lives in appsetup, shared).
	t.Setenv("REIN_MACHINE_HOSTNAME", "toms-really-long-macbook-pro")
	got := resolveMachineLabel("", nonTTY(t), false)
	if len(got) > 10 { // == appsetup.maxLabelLen (label budget)
		t.Errorf("hostname label not capped: %q (len %d)", got, len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("capped label ends in a hyphen: %q", got)
	}
}

func TestResolveMachineLabel_UnusableHostnameFallsBackToDefault(t *testing.T) {
	// A hostname that sanitizes to nothing yields the generic default rather
	// than an empty label (which would break the App name).
	t.Setenv("REIN_MACHINE_HOSTNAME", "!!!")
	if got := resolveMachineLabel("", nonTTY(t), false); got != defaultMachineLabel {
		t.Errorf("empty-hostname fallback = %q, want %q", got, defaultMachineLabel)
	}
}

func TestIsDistinctiveHostname(t *testing.T) {
	for _, generic := range []string{"", "ubuntu", "LOCALHOST", " debian ", "vm"} {
		if isDistinctiveHostname(generic) {
			t.Errorf("hostname %q should be treated as generic", generic)
		}
	}
	for _, distinct := range []string{"toms-laptop", "workvm-42", "buildbox"} {
		if !isDistinctiveHostname(distinct) {
			t.Errorf("hostname %q should be treated as distinctive", distinct)
		}
	}
}
