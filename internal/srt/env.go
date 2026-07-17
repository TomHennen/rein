package srt

import (
	"sort"
	"strings"
)

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
// tracking intent (#102/#119). See EnvParams.UpstreamIntentFile.
const EnvUpstreamIntentFile = "REIN_UPSTREAM_INTENT_FILE"

// EnvDisableClaudeMCP is the rein-side, per-run opt-OUT that restores the old
// behavior of disabling Claude Code's account/claude.ai remote MCP connectors
// (see EnvParams.DisableClaudeAIMCP). By DEFAULT rein no longer disables them —
// claude's native default (connectors enabled, connected non-blocking) applies,
// so a user's MCP servers work when their host is in allow_domains and unreachable
// ones just fail in the background. This var is for operators who want the
// minimal egress/connection surface. Read from rein's OWN launch environment
// OUTSIDE the sandbox; never carried into the injected env as a passthrough.
// Truthy values: "1", "true", "yes", "on" (case-insensitive).
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

	// EnvInSandboxHome is set to "ephemeral" when $HOME is hidden behind srt's
	// deny-read tmpfs (the #59 default), and is ABSENT otherwise. Absent rather
	// than "persistent" so a stale/blank value can never assert the dangerous
	// direction: an agent that sees nothing here assumes its $HOME is real,
	// which is the safe reading when the kill switch is on.
	EnvInSandboxHome = "REIN_IN_SANDBOX_HOME"
)

// EnvParams are the inputs to BuildEnv.
type EnvParams struct {
	// Parent is the environment to draw the passthrough vars FROM (normally
	// os.Environ()). Only the allowlisted names are carried over; everything
	// else — including secrets like ANTHROPIC_API_KEY, AWS_*, GITHUB_TOKEN,
	// SSH_AUTH_SOCK, DBUS_SESSION_BUS_ADDRESS — is dropped.
	Parent []string

	// CABundlePath is the per-run CA bundle (system roots + rein CA). It is
	// pointed at by all four CA env vars so git/curl/node/openssl trust rein's
	// MITM leaf on the inject path while still trusting real certs on the CDN
	// (passthrough) path.
	CABundlePath string

	// StubGHToken is the value for GH_TOKEN inside the sandbox. It is a
	// non-secret placeholder: real gh/git auth is injected by rein's proxy, so
	// the agent never needs a real token, but tools that branch on GH_TOKEN
	// presence behave. Must be non-empty (an empty GH_TOKEN can make gh prompt
	// or fall back to keyring).
	StubGHToken string

	// GitAuthorName / GitAuthorEmail are the NON-IMPERSONATING git identity
	// stamped into GIT_AUTHOR_NAME/EMAIL and GIT_COMMITTER_NAME/EMAIL (CP4).
	// They are the ROBUST authorship mechanism: these env vars override any
	// git config, so a sandboxed `git commit` authors as rein's identity, not
	// the developer whose ~/.gitconfig git would otherwise read. Resolved by
	// internal/gitidentity. When empty (e.g. the VerifyConfigApplied probe,
	// which runs no git), the four vars are simply not set.
	GitAuthorName  string
	GitAuthorEmail string

	// GitConfigGlobalPath, when non-empty, is set as GIT_CONFIG_GLOBAL — a
	// rein-managed per-run gitconfig OUTSIDE the sandbox's view of the
	// developer's ~/.gitconfig. This stops the leak of the developer's email +
	// credential-helper config (which the host ~/.gitconfig would otherwise
	// expose in-sandbox) and is the config-level twin of the GIT_AUTHOR_* env
	// override. GIT_CONFIG_SYSTEM is always pinned to /dev/null so no
	// /etc/gitconfig leaks either (mirrors direct mode, run.go).
	GitConfigGlobalPath string

	// DisableClaudeAIMCP controls the ENABLE_CLAUDEAI_MCP_SERVERS knob for Claude
	// Code. When FALSE (the DEFAULT), rein does NOT set the var, so claude's own
	// default applies: the ACCOUNT/claude.ai remote MCP connectors (Todoist/Gmail/
	// GDrive/GCal, synced from a claude.ai account) are enabled and connected
	// NON-BLOCKING at startup — a user's MCP servers work when their host is in
	// allow_domains, and unreachable ones fail in the background rather than
	// hanging startup. TESTED: with connectors enabled and their hosts NOT in
	// allow_domains, the agent started and answered a prompt normally — startup
	// did not hang. That the account connectors are gated by egress reachability
	// is INFERRED (they must reach claude.ai/provider hosts, which are not in the
	// default allowlist), not directly observed; a local stdio MCP tool, by
	// contrast, was observed callable in-sandbox. When TRUE, rein sets
	// ENABLE_CLAUDEAI_MCP_SERVERS=false, restoring the old minimal-surface behavior
	// (account connectors off). Set via REIN_DISABLE_CLAUDE_MCP.
	//
	// This knob gates ONLY the account/claude.ai remote connectors. USER-configured
	// MCP servers (local stdio via `claude mcp add`, project .mcp.json, settings
	// mcpServers) are a SEPARATE path this env var does not touch — those work in
	// the sandbox regardless (a local stdio server needs no egress; a remote one
	// needs its host in allow_domains). Read OUTSIDE the sandbox; never a
	// passthrough of a parent var into the injected set.
	DisableClaudeAIMCP bool

	// WorkTree, when non-empty, is the agent's repo checkout — the ONLY path
	// that survives the run. Delivered to the child as REIN_IN_SANDBOX_WORKTREE.
	//
	// Direction matters: unlike EnvSandboxShowHome / EnvSandboxAllowRead (which
	// rein READS from its own launch env, OUTSIDE the sandbox), the REIN_IN_SANDBOX_*
	// namespace (see its const block) is WRITTEN BY rein INTO the sandbox for
	// the AGENT to read. They are the only agent-visible channel rein currently
	// has: the launch banner explaining the ephemeral $HOME goes to the HUMAN's
	// terminal, and the agent never sees a word of it (#63).
	//
	// Honest about the limits: this is a breadcrumb, not a guarantee. Claude
	// Code does not surface env vars in its context, so it closes the LOUD case
	// (an agent debugging a weird filesystem failure runs `env`, or a hook/
	// wrapper script branches on it) and NOT the silent one (the agent writes
	// work product to ~/notes.md, reads it back fine all run — the tmpfs is
	// self-consistent — and it evaporates at teardown, where only the human
	// ever notices). The channel that DOES close the silent case is the sandbox
	// contract (cmd/rein/contract.go): claude gets it in its system prompt via
	// --append-system-prompt, other agents get it printed into their output.
	// These vars remain the MACHINE-READABLE form of the same facts, for hooks
	// and wrapper scripts. Non-secret, fixed values.
	WorkTree string

	// HomeEphemeral reports that this run hides $HOME behind srt's deny-read
	// tmpfs (i.e. the #59 default, kill switch NOT engaged). Delivered as
	// REIN_IN_SANDBOX_HOME=ephemeral so an agent that looks can distinguish "my
	// $HOME writes vanish at teardown" from a normal run. See WorkTree.
	HomeEphemeral bool
	// ExtraPathDir, when non-empty, is PREPENDED to the child's PATH. Used
	// to put the per-run staged rein binary (<runTmp>/rein, the probe
	// copy) on the in-sandbox PATH so the agent can run `rein declare <n>`
	// exactly as the deny messages instruct (issue #35 §3). The dir holds
	// only non-secret, already-readable per-run artifacts (rein binary,
	// CA bundle, settings.json, managed gitconfig).
	ExtraPathDir string

	// AgentTmpDir, when non-empty, is the per-run writable scratch dir rein
	// created for the agent and added to srt's allowWrite. It is delivered to the
	// sandboxed child as CLAUDE_CODE_TMPDIR — srt's OWN sanctioned override for
	// the child's TMPDIR (generateProxyEnvVars sets TMPDIR = CLAUDE_CODE_TMPDIR ||
	// CLAUDE_TMPDIR || '/tmp/claude'). rein does NOT set TMPDIR directly: srt owns
	// TMPDIR and overrides it via bwrap --setenv, so a rein-set TMPDIR would be
	// clobbered; CLAUDE_CODE_TMPDIR is the input srt reads to compute it. Without
	// this, srt defaults the child's TMPDIR to /tmp/claude, which it never creates
	// and — because bwrap skips allowWrite sources that don't exist on the host —
	// never binds writable, so the child hits EROFS on its first temp write.
	AgentTmpDir string

	// ClaudeConfigDir, when non-empty, is delivered as CLAUDE_CONFIG_DIR — the
	// rein-owned PERSISTENT overlay claude reads/writes instead of the host's
	// ~/.claude (issue #94). Creds are read EXCLUSIVELY from
	// $CLAUDE_CONFIG_DIR/.credentials.json (spike ground truth: no fixed ~/.claude
	// fallback), so repointing here — with rein having seeded that file host-side
	// and bound the dir read-write via ExtraAllowWrite — is what lets the sandboxed
	// claude authenticate and resume while the host tree stays fully denied.
	// claude-specific but benign to other agents (an env var they ignore); fixed,
	// non-secret path.
	ClaudeConfigDir string

	// RepoWorktrees, when non-empty, is the JSON array of local checkouts bound
	// into this sandbox (worktree.AgentEnvValue). It is delivered as
	// REIN_REPO_WORKTREES — the AGENT-visible answer to "where does repo B
	// live?", which the agent cannot guess and which the human banner alone
	// cannot tell it (issue #64). In-sandbox paths equal host paths. Non-secret:
	// it names directories the agent can already see.
	RepoWorktrees string

	// EphemeralCloneDir, when non-empty, is the writable, per-run scratch dir a
	// repo that enters scope MID-RUN must be cloned into — delivered as
	// REIN_EPHEMERAL_CLONE_DIR. Such a repo has no launch-time bind and never
	// can (bwrap binds are fixed at launch), so its tree is ephemeral and the
	// durable artifact is the push (docs/session-scope-ux-mocks.md §7).
	EphemeralCloneDir string

	// UpstreamIntentFile, when non-empty, is the rendezvous path the rein-git shim
	// appends `git push -u` tracking intent to (REIN_UPSTREAM_INTENT_FILE, bound
	// checkouts only; #102/#119). Internal, not a stable agent API.
	UpstreamIntentFile string
}

// passthroughExact is the allowlist of environment variable NAMES carried from
// the parent unchanged. This is the strict-allowlist gap (#1): srt cannot
// express "unset all but these" (its envVars are a per-name denylist), so rein
// execs srt under an explicit env built here. The load-bearing property tested
// in env_test.go is that NO name outside this set (plus the CA vars + GH_TOKEN
// set below) survives — so a secret in the parent env can never reach the agent.
//
// PATH is REQUIRED (srt's whichSync needs bwrap/socat/rg/bash). HOME/LANG are
// needed by most tooling. TERM is a usability addition for the interactive
// agent path (a terminal type is not a secret); dropping it only degrades TUI
// rendering, so it is included deliberately, not by oversight.
var passthroughExact = map[string]bool{
	"PATH": true,
	"HOME": true,
	"LANG": true,
	"TERM": true,
}

// passthroughPrefix carries any parent var whose name starts with one of these
// prefixes (locale settings: LC_ALL, LC_CTYPE, LC_TIME, …). Prefix rather than
// enumerate because the LC_* set is open-ended and none of them are secrets.
var passthroughPrefix = []string{
	"LC_",
}

// caEnvVars are the four CA-trust variables. On the mitmProxy path srt sets NO
// CA vars (mitmCA is undefined), so rein must point every client's trust store
// at the bundle itself. All four point at the same bundle file.
var caEnvVars = []string{
	"SSL_CERT_FILE",       // openssl / git (OpenSSL build) / python
	"GIT_SSL_CAINFO",      // git explicitly
	"NODE_EXTRA_CA_CERTS", // node-based tooling
	"CURL_CA_BUNDLE",      // curl / libcurl
}

// BuildEnv returns the explicit environment slice ("KEY=VALUE") for exec.Cmd.Env
// on the srt launch. It is an allowlist, not a filter of the parent: the result
// contains ONLY the passthrough vars present in Parent, the four CA vars, the
// stub GH_TOKEN, and (CP4) the rein-set git identity + git-config redirects. The
// output is sorted for deterministic tests and logs.
//
// Explicitly NOT propagated (even if set in Parent): HTTP_PROXY/HTTPS_PROXY/
// NO_PROXY/TMPDIR (srt owns those), GIT_AUTHOR_*/GIT_COMMITTER_*/GIT_CONFIG_*
// (rein sets these itself so a stale parent value can't win), and every
// secret-bearing var. This is the single most valuable gap-closure in CP3
// (gap #1); the git identity extends it in CP4 (non-impersonating commits).
//
// rein sets CLAUDE_CODE_TMPDIR (srt's sanctioned lever for the child's TMPDIR —
// see AgentTmpDir; fixed, non-secret) and, ONLY when the operator opts out via
// DisableClaudeAIMCP, ENABLE_CLAUDEAI_MCP_SERVERS=false (turns off Claude Code's
// account/claude.ai remote MCP connectors). By default that var is NOT set, so
// claude's native default (connectors enabled, non-blocking) applies and a user's
// MCP servers work when their host is in allow_domains. ENABLE_CLAUDEAI_MCP_SERVERS
// is claude-specific but benign to other agents (an unknown env var they ignore);
// its value MUST be the string "false" (not "0") to take effect. Neither carries a
// secret.
func BuildEnv(p EnvParams) []string {
	out := make([]string, 0, len(passthroughExact)+len(caEnvVars)+3)

	for _, kv := range p.Parent {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if !allowedEnvName(name) {
			continue
		}
		// Prepend the staged-binary dir to PATH (issue #35: `rein declare`
		// must resolve in-sandbox as the deny messages instruct).
		if name == "PATH" && p.ExtraPathDir != "" {
			kv = "PATH=" + p.ExtraPathDir + ":" + value
		}
		out = append(out, kv)
	}
	for _, name := range caEnvVars {
		out = append(out, name+"="+p.CABundlePath)
	}
	out = append(out, "GH_TOKEN="+p.StubGHToken)

	// Git identity (CP4). Set author AND committer to the same non-impersonating
	// value. These override git config, so they are the authorship guarantee
	// regardless of what config the sandbox can read. Set only when resolved
	// (the probe path passes them empty and needs no identity).
	if p.GitAuthorName != "" {
		out = append(out, "GIT_AUTHOR_NAME="+p.GitAuthorName)
		out = append(out, "GIT_COMMITTER_NAME="+p.GitAuthorName)
	}
	if p.GitAuthorEmail != "" {
		out = append(out, "GIT_AUTHOR_EMAIL="+p.GitAuthorEmail)
		out = append(out, "GIT_COMMITTER_EMAIL="+p.GitAuthorEmail)
	}
	// Redirect git's global config away from the developer's ~/.gitconfig (stops
	// the email + credential-helper leak) and pin the system config to /dev/null.
	// GIT_CONFIG_SYSTEM is set unconditionally — it is safe and desirable even on
	// the probe path (no /etc/gitconfig should ever influence the sandbox).
	if p.GitConfigGlobalPath != "" {
		out = append(out, "GIT_CONFIG_GLOBAL="+p.GitConfigGlobalPath)
	}
	out = append(out, "GIT_CONFIG_SYSTEM=/dev/null")

	// Per-run writable scratch dir → srt's CLAUDE_CODE_TMPDIR lever (see AgentTmpDir).
	// Set only when provided (the probe path passes it empty and does no temp work).
	if p.AgentTmpDir != "" {
		out = append(out, "CLAUDE_CODE_TMPDIR="+p.AgentTmpDir)
	}
	// Rein-owned persistent CLAUDE_CONFIG_DIR overlay (#94). Set only when provided
	// (the probe path passes it empty). claude reads creds/settings from and writes
	// its session into this dir instead of the fully-denied host ~/.claude.
	if p.ClaudeConfigDir != "" {
		out = append(out, "CLAUDE_CONFIG_DIR="+p.ClaudeConfigDir)
	}
	// Where the developer's real checkouts are mounted, and where to clone a
	// repo that only enters scope mid-run (issue #64). Both are non-secret
	// facts about the sandbox's own filesystem; the agent cannot infer either.
	if p.RepoWorktrees != "" {
		out = append(out, EnvAgentWorktrees+"="+p.RepoWorktrees)
	}
	if p.EphemeralCloneDir != "" {
		out = append(out, EnvAgentCloneDir+"="+p.EphemeralCloneDir)
	}
	if p.UpstreamIntentFile != "" {
		out = append(out, EnvUpstreamIntentFile+"="+p.UpstreamIntentFile)
	}

	// Agent-visible facts (#63). rein WRITES these INTO the sandbox — see the
	// WorkTree field doc for why (the banner reaches the human, never the agent)
	// and for the honest limits. All three are non-secret, fixed values.
	out = append(out, EnvInSandbox+"=1")
	if p.WorkTree != "" {
		out = append(out, EnvInSandboxWorktree+"="+p.WorkTree)
	}
	if p.HomeEphemeral {
		out = append(out, EnvInSandboxHome+"=ephemeral")
	}
	// By default rein leaves Claude Code's account/claude.ai MCP connectors at
	// claude's native setting (enabled, connected non-blocking): a user's MCP
	// servers work when their host is in allow_domains, and unreachable ones fail
	// in the background without hanging startup. Only when the operator opts OUT
	// (REIN_DISABLE_CLAUDE_MCP → DisableClaudeAIMCP) does rein force the connectors
	// off. Non-secret; value must be exactly "false" (not "0") to take effect.
	if p.DisableClaudeAIMCP {
		out = append(out, "ENABLE_CLAUDEAI_MCP_SERVERS=false")
	}

	sort.Strings(out)
	return out
}

// allowedEnvName reports whether a parent env var name is on the passthrough
// allowlist. The CA vars and GH_TOKEN are set explicitly by BuildEnv (not
// carried from the parent), so they are NOT allowlisted here — a stale value in
// the parent must not shadow the value rein sets.
func allowedEnvName(name string) bool {
	if passthroughExact[name] {
		return true
	}
	for _, pre := range passthroughPrefix {
		if strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}
