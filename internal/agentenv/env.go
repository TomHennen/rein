// Package agentenv holds the agent-visible env-var contract (names + the
// small parsing helper) that is substrate-neutral: not specific to srt's
// bwrap/mount model, so a future nono launcher can set the same vars without
// importing internal/srt. Extracted from internal/srt/env.go per
// docs/design-nono-pivot.md §5/§7.
package agentenv

import "strings"

// EnvAgentWorktrees is the AGENT-visible channel: a JSON array of
// {"repo","path","mode"} objects naming every local checkout bound into this
// sandbox and where it is mounted (issue #64). The agent cannot guess where the
// developer's checkout of repo B lives — this is how it is told. In-sandbox
// paths equal host paths (srt bind-mounts same-path). The value is rendered by
// internal/worktree.AgentEnvValue.
const EnvAgentWorktrees = "REIN_REPO_WORKTREES"

// EnvAgentCloneDir is the AGENT-visible writable, EPHEMERAL directory to clone
// into when a repo enters scope MID-RUN (docs/session-scope-ux-mocks.md §7):
// bwrap binds are fixed at launch, so such a repo has no bind and never can.
// Its tree lives here for the run and the durable artifact is the push, not the
// tree. Explicitly NOT the working tree — a nested clone inside repo A risks
// being committed into repo A.
const EnvAgentCloneDir = "REIN_EPHEMERAL_CLONE_DIR"

// EnvUpstreamIntentFile is the shim-internal rendezvous for `git push -u`
// tracking intent (#102/#119). See srt.EnvParams.UpstreamIntentFile.
const EnvUpstreamIntentFile = "REIN_UPSTREAM_INTENT_FILE"

// EnvDisableClaudeMCP is the rein-side, per-run opt-OUT that restores the old
// behavior of disabling Claude Code's account/claude.ai remote MCP connectors
// (see srt.EnvParams.DisableClaudeAIMCP). By DEFAULT rein no longer disables
// them — claude's native default (connectors enabled, connected non-blocking)
// applies, so a user's MCP servers work when their host is in allow_domains and
// unreachable ones just fail in the background. This var is for operators who
// want the minimal egress/connection surface. Read from rein's OWN launch
// environment OUTSIDE the sandbox; never carried into the injected env as a
// passthrough. Truthy values: "1", "true", "yes", "on" (case-insensitive).
const EnvDisableClaudeMCP = "REIN_DISABLE_CLAUDE_MCP"

// DisableClaudeMCPFromEnv reports whether the given value of REIN_DISABLE_CLAUDE_MCP
// opts OUT of claude's account/claude.ai MCP connectors. Only an explicit truthy
// value disables; anything else (unset, empty, "0", "false", garbage) keeps the
// default (MCP enabled).
func DisableClaudeMCPFromEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// The REIN_IN_SANDBOX_* namespace is the one rein WRITES INTO the sandbox for
// the AGENT to read (#63). It is deliberately NOT REIN_SANDBOX_*: that prefix
// is already rein's READ-from-its-own-environment namespace, set by the human
// OUTSIDE the sandbox (REIN_SANDBOX_SHOW_HOME, REIN_SANDBOX_ALLOW_READ,
// REIN_SANDBOX_WORKDIR). Two opposite data-flow directions under one prefix is
// a trap — REIN_SANDBOX_HOME (a fact for the agent) sitting one word from
// REIN_SANDBOX_SHOW_HOME (a knob for the human) is exactly the confusion that
// prefix would invite. Read the prefix as the sentence it forms: "rein: in
// sandbox, <fact>". Verified live that srt/bwrap pass these through unchanged.
const (
	// EnvInSandbox is the plain "you are running inside a rein sandbox"
	// primitive: "1", always set. For agents, hooks, and wrapper scripts that
	// want to branch without parsing anything else.
	EnvInSandbox = "REIN_IN_SANDBOX"

	// EnvInSandboxWorktree is the agent's repo checkout — the ONLY path that
	// survives the run. The single most useful fact rein can hand an agent
	// whose $HOME evaporates: this is where work has to go.
	EnvInSandboxWorktree = "REIN_IN_SANDBOX_WORKTREE"

	// EnvInSandboxHome is set to "ephemeral" when $HOME is hidden behind the
	// sandbox's deny-read tmpfs/Landlock rule (the #59 default), and is ABSENT
	// otherwise. Absent rather than "persistent" so a stale/blank value can
	// never assert the dangerous direction: an agent that sees nothing here
	// assumes its $HOME is real, which is the safe reading when the kill
	// switch is on.
	EnvInSandboxHome = "REIN_IN_SANDBOX_HOME"
)
