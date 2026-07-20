// The SANDBOX CONTRACT: the single source of truth for what the AGENT must
// know about the environment rein put it in (#63, closing the education layer
// #35 deliberately deferred as revisit-on-dogfood).
//
// Why this exists at all: everything rein tells the operator — the launch
// banner, the approval prompts — goes to the HUMAN's terminal. The agent never
// sees a word of it. Before this, an agent learned the rules only REACTIVELY,
// by hitting a deny message (#35's tier 1), and learned the ephemeral-$HOME
// rules not at all: writes to $HOME succeed, read back fine all run, and
// evaporate at teardown, so nothing ever tells the agent its work is doomed
// (#63). This is the proactive layer.
//
// One injection carries the WHOLE contract — filesystem, credentials, the
// declare ceremony, egress — because the agent needs all of it up front and a
// second channel is a second thing to keep in sync.
//
// The declare/branch wording here is COPIED from the strings the agent actually
// hits in #35's deny messages (internal/proxy/gate.go, internal/declare) — the
// contract must not teach a different vocabulary than the errors do, or a
// confused agent gets two stories.
package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnvDisableAgentContract turns the injection OFF. Named to match the existing
// opt-out (REIN_DISABLE_CLAUDE_MCP); truthy values are "1", "true", "yes", "on".
// When set, the banner says so LOUDLY — a silently un-briefed agent is exactly
// the failure this feature exists to fix.
const EnvDisableAgentContract = "REIN_DISABLE_AGENT_CONTRACT"

// contractParams are the run facts the contract states. Everything here is
// non-secret and already visible to the agent by observation — the contract
// just saves it the trouble of discovering the rules by breaking things.
type contractParams struct {
	// WorkTree is the working directory the agent starts in. Normally the ONLY
	// path that survives the run; when WorkTreeEphemeral is set it is itself a
	// throwaway (see below).
	WorkTree string
	// HomeEphemeral is true when $HOME is hidden in-sandbox (under nono's
	// default-deny filesystem, always). If a future mode leaves $HOME readable,
	// this must be false so the contract omits the $HOME clauses rather than
	// telling the agent a LIE.
	HomeEphemeral bool
	// WorkTreeEphemeral is true when the developer's real cwd checkout could NOT
	// be safely bound (a submodule superproject or a linked worktree — its `.git`
	// cannot be hardened against the host-exec escape, issue #64), so rein gave
	// the agent a fresh EMPTY scratch tree instead and did NOT bind the real one.
	// The contract must then tell the agent the truth the "your work persists in
	// the working tree" line would otherwise invert: NOTHING local survives — it
	// must clone WorkTreeRepo here and PUSH.
	WorkTreeEphemeral bool
	// WorkTreeRepo is the owner/name the cwd is a checkout of (when detected).
	// Used only to name what to clone in the ephemeral-worktree message.
	WorkTreeRepo string
	// ExtraDomains are the operator's extra egress hosts (beyond GitHub).
	ExtraDomains []string
}

// buildAgentContract renders the contract. Terse and factual on purpose: this
// is system-prompt real estate, and a wall of prose gets skimmed. Every line is
// a rule the agent can act on, not background.
func buildAgentContract(p contractParams) string {
	var b strings.Builder

	b.WriteString("rein sandbox: you are running inside a sandbox managed by rein. These rules are ENFORCED, not advice.\n")

	b.WriteString("\nFILES\n")
	if p.WorkTreeEphemeral {
		// The working tree itself is a throwaway — do NOT tell the agent its work
		// persists there; that is exactly backwards. It must clone and push.
		fmt.Fprintf(&b, "- Your working tree %s is an EMPTY, TEMPORARY directory, NOT a checkout.\n", p.WorkTree)
		if p.WorkTreeRepo != "" {
			fmt.Fprintf(&b, "  Your real checkout of %s could not be safely bound, so clone it here:\n", p.WorkTreeRepo)
			fmt.Fprintf(&b, "      git clone https://github.com/%s .\n", p.WorkTreeRepo)
		} else {
			b.WriteString("  Clone the repository you are working on here (git clone <url> .).\n")
		}
		b.WriteString("- NOTHING you write locally survives this run — not even this tree. The ONLY\n")
		b.WriteString("  durable artifact is a PUSH to GitHub. Commit and push your work, or you lose it.\n")
	} else {
		fmt.Fprintf(&b, "- Your work persists ONLY in the working tree: %s\n", p.WorkTree)
	}
	if p.HomeEphemeral {
		// The "write it THERE" pointer only makes sense when the working tree is a
		// durable place to write. In the ephemeral-worktree case there is none —
		// the message above already told the agent the only durable path is a push.
		if !p.WorkTreeEphemeral {
			b.WriteString("  Write anything that must outlive this run THERE. Nothing else survives.\n")
		}
		b.WriteString("- $HOME is EPHEMERAL. Writes under it SUCCEED and read back normally, then are\n")
		b.WriteString("  DISCARDED when the run ends. You will get no error. Do not keep work there.\n")
		b.WriteString("- Most of $HOME reads as empty/missing; a few paths are re-exposed READ-ONLY, where\n")
		b.WriteString("  writes fail with \"Read-only file system\". Caches work but start cold each run.\n")
	} else if !p.WorkTreeEphemeral {
		b.WriteString("  Write anything that must outlive this run THERE.\n")
	}

	b.WriteString("\nCREDENTIALS\n")
	b.WriteString("- There are NO credentials in this environment. Tokens, keys, and credential stores\n")
	b.WriteString("  are hidden on purpose. Do not go looking for them, and do not ask the user for one.\n")
	b.WriteString("- git and gh still work anyway: a proxy OUTSIDE the sandbox injects a short-lived\n")
	b.WriteString("  GitHub token you never see. Just use them normally.\n")

	b.WriteString("\nGITHUB WRITES\n")
	b.WriteString("- Writes to GitHub (push, PRs, comments, any API mutation) are LOCKED until you\n")
	b.WriteString("  declare the issue this work is for:\n")
	b.WriteString("      rein declare <n>\n")
	b.WriteString("  That asks the human to approve, on their terminal. Reads work without it.\n")
	b.WriteString("- Once confirmed, push ONLY to a branch named:\n")
	b.WriteString("      agent/<n>/<nonce>\n")
	b.WriteString("  where <n> is the declared issue number and <nonce> is a short name you choose\n")
	b.WriteString("  (letters/digits, then letters/digits/./_/-). Any other ref is rejected.\n")
	b.WriteString("- One issue per push.\n")

	b.WriteString("\nNETWORK\n")
	if len(p.ExtraDomains) > 0 {
		fmt.Fprintf(&b, "- Egress is restricted to GitHub plus: %s\n", strings.Join(p.ExtraDomains, ", "))
	} else {
		b.WriteString("- Egress is restricted to GitHub.\n")
	}
	b.WriteString("- Every other host is unreachable. A failed fetch is the sandbox, not a flaky network.\n")

	return b.String()
}

// isClaudeCommand reports whether argv0 launches Claude Code, i.e. whether the
// --append-system-prompt channel is available.
//
// Robustness rules, in order:
//   - basename match on the literal argv0 ("claude", "/opt/x/claude", "./claude");
//   - then PATH-resolve and symlink-resolve, and match the basename of the TARGET
//     — this catches the native installer's launcher (~/.local/bin/claude ->
//     .../versions/<v>/claude) and npm bin symlinks.
//
// Deliberately conservative: a wrapper script under some other name is NOT
// treated as claude. A false positive would hand `--append-system-prompt` to a
// program that does not understand it and BREAK THE RUN; a false negative just
// falls back to printing the contract, which is harmless. So when in doubt: no.
func isClaudeCommand(argv0 string) bool {
	if argv0 == "" {
		return false
	}
	if claudeBase(argv0) {
		return true
	}
	found, err := exec.LookPath(argv0)
	if err != nil {
		return false
	}
	if claudeBase(found) {
		return true
	}
	target, err := filepath.EvalSymlinks(found)
	if err != nil {
		return false
	}
	return claudeBase(target)
}

func claudeBase(p string) bool {
	return filepath.Base(strings.TrimSpace(p)) == "claude"
}

// injectContract returns the argv to actually launch. For claude it inserts
// --append-system-prompt <contract> immediately after argv0 — before any user
// args, so a trailing positional prompt (`claude -p "do the thing"`) is not
// separated from its flag. For every other agent the argv is returned unchanged
// and the caller prints the contract instead (there is no general per-agent
// system-prompt mechanism; see the banner line).
//
// injected reports which channel was used, so the banner can tell the operator
// the truth rather than implying the agent was briefed when it was not.
func injectContract(cmdline []string, contract string) (argv []string, injected bool) {
	if len(cmdline) == 0 || !isClaudeCommand(cmdline[0]) {
		return cmdline, false
	}
	out := make([]string, 0, len(cmdline)+2)
	out = append(out, cmdline[0], "--append-system-prompt", contract)
	out = append(out, cmdline[1:]...)
	return out, true
}

// contractStatus renders the ONE banner line saying how (or whether) the agent
// was briefed with the contract above: injected into claude's system prompt,
// printed to output, or DISABLED via the kill switch.
func contractStatus(off, injected bool) string {
	switch {
	case off:
		return "  WARNING: agent contract DISABLED (" + EnvDisableAgentContract + ") — the agent was NOT told that $HOME is\n    ephemeral, that credentials are absent, or how to declare its issue. It will find out by failing."
	case injected:
		return "  agent contract injected via --append-system-prompt (claude): $HOME is ephemeral, no creds, declare-then-push."
	default:
		return "  agent contract PRINTED to the agent's output (this agent has no system-prompt channel, so it may or\n    may not reach the model's context; the REIN_IN_SANDBOX_* env vars carry the same facts machine-readably)."
	}
}
