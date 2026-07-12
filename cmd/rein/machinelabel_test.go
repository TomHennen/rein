package main

import (
	"os"
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
	// prompting — never block (guardrail §7).
	t.Setenv("REIN_MACHINE_HOSTNAME", "Toms-Laptop")
	if got := resolveMachineLabel("", nonTTY(t), false); got != "toms-laptop" {
		t.Errorf("hostname fallback = %q, want toms-laptop", got)
	}
	// --yes also forces the non-interactive fallback even on a tty.
	if got := resolveMachineLabel("", nonTTY(t), true); got != "toms-laptop" {
		t.Errorf("--yes fallback = %q, want toms-laptop", got)
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
