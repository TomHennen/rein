package srt

import (
	"sort"
	"strings"
)

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
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if allowedEnvName(name) {
			out = append(out, kv)
		}
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
