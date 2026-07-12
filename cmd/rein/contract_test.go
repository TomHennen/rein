package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildAgentContract_StatesTheEnforcedRules pins the facts the contract MUST
// carry. Each assertion maps to a rule the agent would otherwise discover only by
// breaking something — and, for the $HOME clause, would never discover at all
// (writes there succeed and read back fine; they only evaporate at teardown,
// where the agent is already gone).
//
// The declare/branch vocabulary is asserted VERBATIM against the strings the
// agent actually hits in #35's deny messages (internal/proxy/gate.go). If those
// ever change, this test fails and the contract gets fixed with them — a
// contract that teaches different words than the errors use is worse than none.
func TestBuildAgentContract_StatesTheEnforcedRules(t *testing.T) {
	got := buildAgentContract(contractParams{
		WorkTree:      "/work/repo",
		HomeEphemeral: true,
		ExtraDomains:  []string{"api.anthropic.com", "registry.npmjs.org"},
	})

	for _, want := range []string{
		"/work/repo", // where work persists — the single most load-bearing fact
		"$HOME is EPHEMERAL",
		"DISCARDED when the run ends",
		"no error", // the silent-failure warning: writes SUCCEED, so nothing else warns it
		"Read-only file system",
		"NO credentials",
		"rein declare <n>",   // exact #35 vocabulary (gate.go deny messages)
		"agent/<n>/<nonce>",  // exact #35 branch convention
		"One issue per push", // exact #35 push rule
		"api.anthropic.com, registry.npmjs.org",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("contract missing %q; the agent would not know this rule.\n--- contract ---\n%s", want, got)
		}
	}
}

// TestBuildAgentContract_KillSwitchTellsNoLie: under REIN_SANDBOX_SHOW_HOME the
// $HOME deny is OFF, so $HOME is a real, persistent home. Telling the agent its
// $HOME is ephemeral would then be a LIE that could make it refuse to use a
// perfectly good cache — or, worse, distrust the rest of the contract.
func TestBuildAgentContract_KillSwitchTellsNoLie(t *testing.T) {
	got := buildAgentContract(contractParams{WorkTree: "/work/repo", HomeEphemeral: false})
	for _, forbidden := range []string{"EPHEMERAL", "DISCARDED"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("contract claims %q while the $HOME deny is OFF — that is false.\n--- contract ---\n%s", forbidden, got)
		}
	}
	// The persistence rule still holds and must still be stated.
	if !strings.Contains(got, "/work/repo") {
		t.Errorf("contract must still name the working tree; got:\n%s", got)
	}
}

// TestInjectContract_ClaudeVsOther is the branch that decides whether the agent
// is briefed for real (system prompt) or only best-effort (printed output).
//
// The false-positive direction is the dangerous one: handing
// --append-system-prompt to an agent that does not understand the flag BREAKS
// THE RUN. A false negative merely falls back to printing. So detection must be
// conservative, and this test pins that asymmetry.
func TestInjectContract_ClaudeVsOther(t *testing.T) {
	const contract = "CONTRACT-BODY"

	// A bare `claude` resolves via PATH on this box; use an explicit path so the
	// test does not depend on claude being installed.
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	argv, injected := injectContract([]string{claudePath, "-p", "do the thing"}, contract)
	if !injected {
		t.Fatalf("claude at %q was not detected; the agent would never see the contract", claudePath)
	}
	// The flag must land immediately after argv0 — NOT at the end, where it would
	// separate `-p` from its positional prompt.
	want := []string{claudePath, "--append-system-prompt", contract, "-p", "do the thing"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %v, want %v", argv, want)
		}
	}

	// A NON-claude agent must be left completely alone: an unknown flag would
	// break its launch outright.
	other := filepath.Join(dir, "some-other-agent")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	argv, injected = injectContract([]string{other, "--flag"}, contract)
	if injected {
		t.Errorf("non-claude agent %q was treated as claude — --append-system-prompt would break it", other)
	}
	if len(argv) != 2 || argv[0] != other || argv[1] != "--flag" {
		t.Errorf("non-claude argv was modified: %v", argv)
	}

	// Empty argv must not panic or inject.
	if _, inj := injectContract(nil, contract); inj {
		t.Errorf("empty argv reported as injected")
	}
}

// TestIsClaudeCommand_ResolvesInstallChains: the native installer ships
// ~/.local/bin/claude as a SYMLINK to .../versions/<v>/claude, and npm ships a
// bin symlink — detection must follow both, or a normally-installed claude
// silently falls back to the weaker printed channel.
func TestIsClaudeCommand_ResolvesInstallChains(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "claude")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink NAMED claude pointing at the real one: basename matches directly.
	link := filepath.Join(dir, "claude-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// claude-link's basename is not "claude", but its symlink TARGET's is.
	if !isClaudeCommand(link) {
		t.Errorf("isClaudeCommand(%q) = false; the symlink target %q is claude, so the install-chain "+
			"resolution failed and a real claude install would miss its system-prompt channel", link, real)
	}
	if !isClaudeCommand(real) {
		t.Errorf("isClaudeCommand(%q) = false for a literal claude path", real)
	}
	// Conservative: something that merely CONTAINS "claude" is not claude.
	notClaude := filepath.Join(dir, "claude-wrapper")
	if err := os.WriteFile(notClaude, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isClaudeCommand(notClaude) {
		t.Errorf("isClaudeCommand(%q) = true; a wrapper is not claude and would break on the flag", notClaude)
	}
	if isClaudeCommand("") {
		t.Errorf("isClaudeCommand(\"\") = true")
	}
}

// TestContractStatus_TellsTheOperatorTheTruth: the banner line must distinguish
// "the agent was actually briefed" from "we printed something and hoped". Without
// it, an operator cannot tell whether their agent knows the rules.
func TestContractStatus_TellsTheOperatorTheTruth(t *testing.T) {
	if s := contractStatus(false, true); !strings.Contains(s, "--append-system-prompt") {
		t.Errorf("injected status should name the channel; got %q", s)
	}
	if s := contractStatus(false, false); !strings.Contains(s, "PRINTED") {
		t.Errorf("printed status should say the contract was only printed; got %q", s)
	}
	// The disable hatch must be LOUD — a silently un-briefed agent is the exact
	// failure this feature exists to prevent.
	s := contractStatus(true, false)
	if !strings.Contains(s, "WARNING") || !strings.Contains(s, EnvDisableAgentContract) {
		t.Errorf("disabled status must warn loudly and name the env var; got %q", s)
	}
}

// TestContractEphemeralWorkTree (#64): when the working tree is an ephemeral
// throwaway (unhardenable cwd fallback), the contract must tell the AGENT to
// clone-and-push and that nothing local survives — NOT the default "your work
// persists in the working tree", which is exactly backwards and would lose the
// agent's work.
func TestContractEphemeralWorkTree(t *testing.T) {
	c := buildAgentContract(contractParams{
		WorkTree:          "/tmp/rein-ephemeral-work-1",
		HomeEphemeral:     true,
		WorkTreeEphemeral: true,
		WorkTreeRepo:      "owner/super",
	})
	for _, want := range []string{
		"EMPTY, TEMPORARY directory",
		"git clone https://github.com/owner/super .",
		"NOTHING you write locally survives",
		"durable artifact is a PUSH",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("ephemeral contract missing %q; got:\n%s", want, c)
		}
	}
	// The default persistence promise must be ABSENT — it is the lie we're avoiding.
	if strings.Contains(c, "Your work persists ONLY in the working tree") {
		t.Errorf("ephemeral contract must not promise the working tree persists; got:\n%s", c)
	}
	if strings.Contains(c, "must outlive this run THERE") {
		t.Errorf("ephemeral contract must not point at the (throwaway) working tree as durable; got:\n%s", c)
	}

	// Sanity: the NON-ephemeral contract still makes the persistence promise.
	c = buildAgentContract(contractParams{WorkTree: "/work/repo", HomeEphemeral: true})
	if !strings.Contains(c, "Your work persists ONLY in the working tree: /work/repo") {
		t.Errorf("non-ephemeral contract lost its persistence promise; got:\n%s", c)
	}
}
