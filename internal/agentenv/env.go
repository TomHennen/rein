// Package agentenv holds the substrate-neutral agent-visible env-var contract
// (names + a parse helper), so the nono launcher sets the same vars without
// reaching into any backend package. Established per docs/design-nono-pivot.md §5/§7.
package agentenv

import "strings"

// EnvAgentWorktrees: JSON array of {"repo","path","mode"} naming every checkout
// bound into the sandbox and its mount point (#64). Rendered by worktree.AgentEnvValue.
const EnvAgentWorktrees = "REIN_REPO_WORKTREES"

// EnvAgentCloneDir: writable ephemeral dir to clone into when a repo enters scope
// mid-run (bwrap binds are fixed at launch). Not the working tree.
const EnvAgentCloneDir = "REIN_EPHEMERAL_CLONE_DIR"

// EnvUpstreamIntentFile: shim-internal rendezvous for `git push -u` tracking intent
// (#102/#119).
const EnvUpstreamIntentFile = "REIN_UPSTREAM_INTENT_FILE"

// EnvDisableClaudeMCP: per-run opt-OUT restoring the old behavior of disabling
// Claude Code's claude.ai MCP connectors (default is now enabled). Read from rein's
// own launch env outside the sandbox; never passed through. Truthy: 1/true/yes/on.
const EnvDisableClaudeMCP = "REIN_DISABLE_CLAUDE_MCP"

// DisableClaudeMCPFromEnv reports whether v opts out of the MCP connectors. Only an
// explicit truthy value disables; anything else keeps the default (enabled).
func DisableClaudeMCPFromEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// REIN_IN_SANDBOX_* is what rein WRITES INTO the sandbox for the agent to read (#63).
// Distinct from REIN_SANDBOX_* (rein's own read-from-environment knobs, set by the
// human outside the sandbox) — opposite data-flow directions, kept under separate prefixes.
const (
	// EnvInSandbox: "you are inside a rein sandbox" primitive ("1", always set).
	EnvInSandbox = "REIN_IN_SANDBOX"

	// EnvInSandboxWorktree: the agent's repo checkout — the only path that survives the run.
	EnvInSandboxWorktree = "REIN_IN_SANDBOX_WORKTREE"

	// EnvInSandboxHome: "ephemeral" when $HOME is hidden (the #59 default), ABSENT
	// otherwise — absent so a blank value never asserts "$HOME is real" falsely.
	EnvInSandboxHome = "REIN_IN_SANDBOX_HOME"
)
