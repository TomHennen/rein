package srt

import (
	"strings"
	"testing"
)

func TestBuildEnvIsStrictAllowlist(t *testing.T) {
	dirty := []string{
		// Secrets / ambient auth that MUST be dropped.
		"ANTHROPIC_API_KEY=sk-ant-secret",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
		"AWS_ACCESS_KEY_ID=akid",
		"GITHUB_TOKEN=ghp_real",
		"GH_ENTERPRISE_TOKEN=ent",
		"SSH_AUTH_SOCK=/run/user/1000/ssh-agent",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
		"GPG_AGENT_INFO=/x",
		"HTTP_PROXY=http://evil:8080",
		"HTTPS_PROXY=http://evil:8080",
		"NO_PROXY=localhost",
		"TMPDIR=/tmp/attacker",
		// Allowlisted passthrough.
		"PATH=/usr/bin:/bin",
		"HOME=/home/dev",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		"TERM=xterm-256color",
		// A parent GH_TOKEN must NOT survive — BuildEnv sets the stub itself.
		"GH_TOKEN=ghp_parent_should_not_win",
		// A parent SSL_CERT_FILE must NOT survive — BuildEnv sets the bundle.
		"SSL_CERT_FILE=/parent/wrong.pem",
		// Parent git identity/config must NOT survive — rein sets its own (or,
		// as here with no identity supplied, drops the author vars entirely).
		"GIT_AUTHOR_NAME=Attacker",
		"GIT_AUTHOR_EMAIL=attacker@evil.test",
		"GIT_CONFIG_GLOBAL=/parent/wrong-gitconfig",
	}
	env := BuildEnv(EnvParams{
		Parent:       dirty,
		CABundlePath: "/run/ca-bundle.pem",
		StubGHToken:  "stub-tok",
	})

	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := got[k]; dup {
			t.Errorf("duplicate env var %q", k)
		}
		got[k] = v
	}

	// Forbidden names must be entirely absent.
	forbidden := []string{
		"ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID",
		"GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "SSH_AUTH_SOCK",
		"DBUS_SESSION_BUS_ADDRESS", "GPG_AGENT_INFO",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "TMPDIR",
		// No identity supplied -> author vars absent; a parent GIT_CONFIG_GLOBAL
		// must never leak through (rein owns it).
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_CONFIG_GLOBAL",
	}
	for _, name := range forbidden {
		if _, ok := got[name]; ok {
			t.Errorf("forbidden env var %q leaked into sandbox env", name)
		}
	}

	// Allowlisted passthrough present with parent values.
	wantPass := map[string]string{
		"PATH":   "/usr/bin:/bin",
		"HOME":   "/home/dev",
		"LANG":   "en_US.UTF-8",
		"LC_ALL": "en_US.UTF-8",
		"TERM":   "xterm-256color",
	}
	for k, v := range wantPass {
		if got[k] != v {
			t.Errorf("passthrough %q = %q, want %q", k, got[k], v)
		}
	}

	// CA vars all point at the bundle (parent value overridden).
	for _, name := range caEnvVars {
		if got[name] != "/run/ca-bundle.pem" {
			t.Errorf("%s = %q, want the bundle path", name, got[name])
		}
	}

	// Stub GH_TOKEN wins over the parent's.
	if got["GH_TOKEN"] != "stub-tok" {
		t.Errorf("GH_TOKEN = %q, want the stub (parent value must not win)", got["GH_TOKEN"])
	}

	// GIT_CONFIG_SYSTEM is pinned to /dev/null unconditionally (no /etc/gitconfig
	// leak), even when no identity is supplied.
	if got["GIT_CONFIG_SYSTEM"] != "/dev/null" {
		t.Errorf("GIT_CONFIG_SYSTEM = %q, want /dev/null", got["GIT_CONFIG_SYSTEM"])
	}

	// By DEFAULT (DisableClaudeAIMCP false) rein does NOT set
	// ENABLE_CLAUDEAI_MCP_SERVERS — claude's native default (connectors enabled,
	// non-blocking) applies so a user's MCP servers work when their host is in
	// allow_domains. The opt-out arm is asserted in TestBuildEnvDisableClaudeMCP.
	if _, ok := got["ENABLE_CLAUDEAI_MCP_SERVERS"]; ok {
		t.Errorf("ENABLE_CLAUDEAI_MCP_SERVERS present by default (should only appear when DisableClaudeAIMCP): %q", got["ENABLE_CLAUDEAI_MCP_SERVERS"])
	}

	// No AgentTmpDir supplied here -> CLAUDE_CODE_TMPDIR must be absent (the probe
	// path does no temp work). It is present only when a scratch dir is provided
	// (asserted in TestBuildEnvAgentTmpDir).
	if _, ok := got["CLAUDE_CODE_TMPDIR"]; ok {
		t.Errorf("CLAUDE_CODE_TMPDIR present with no AgentTmpDir: %q", got["CLAUDE_CODE_TMPDIR"])
	}

	// No WorkTree / HomeEphemeral supplied here -> the two agent-visible facts
	// that carry a value must be absent (REIN_IN_SANDBOX itself is unconditional).
	// Asserted positively in TestBuildEnvAgentVisibleFacts.
	for _, name := range []string{"REIN_IN_SANDBOX_WORKTREE", "REIN_IN_SANDBOX_HOME"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s present with no WorkTree/HomeEphemeral: %q", name, got[name])
		}
	}

	// The full set is ONLY passthrough + CA vars + GH_TOKEN + GIT_CONFIG_SYSTEM +
	// REIN_IN_SANDBOX (no identity or AgentTmpDir supplied here, and MCP not disabled
	// by default) — nothing else.
	allowed := map[string]bool{"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "TERM": true, "GH_TOKEN": true, "GIT_CONFIG_SYSTEM": true, "REIN_IN_SANDBOX": true}
	for _, name := range caEnvVars {
		allowed[name] = true
	}
	for k := range got {
		if !allowed[k] {
			t.Errorf("unexpected env var %q in sandbox env", k)
		}
	}
}

// TestBuildEnvDisableClaudeMCP asserts the opt-OUT arm: when DisableClaudeAIMCP
// is set, BuildEnv emits ENABLE_CLAUDEAI_MCP_SERVERS=false (the string "false",
// not "0"), restoring the minimal-surface behavior; the closed allowlist then
// includes exactly that one extra name.
func TestBuildEnvDisableClaudeMCP(t *testing.T) {
	env := BuildEnv(EnvParams{
		Parent:             []string{"HOME=/home/dev"},
		CABundlePath:       "/run/ca-bundle.pem",
		StubGHToken:        "stub-tok",
		DisableClaudeAIMCP: true,
	})
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["ENABLE_CLAUDEAI_MCP_SERVERS"] != "false" {
		t.Errorf("ENABLE_CLAUDEAI_MCP_SERVERS = %q, want \"false\" when DisableClaudeAIMCP set", got["ENABLE_CLAUDEAI_MCP_SERVERS"])
	}
	// Closed set for this arm: HOME + CA vars + GH_TOKEN + GIT_CONFIG_SYSTEM +
	// REIN_IN_SANDBOX + the one MCP-disable var. Nothing else.
	allowed := map[string]bool{"HOME": true, "GH_TOKEN": true, "GIT_CONFIG_SYSTEM": true, "ENABLE_CLAUDEAI_MCP_SERVERS": true, "REIN_IN_SANDBOX": true}
	for _, name := range caEnvVars {
		allowed[name] = true
	}
	for k := range got {
		if !allowed[k] {
			t.Errorf("unexpected env var %q in disable-MCP sandbox env", k)
		}
	}
}

// TestDisableClaudeMCPFromEnv covers the truthy parser: only explicit truthy
// values opt out; everything else (unset/empty/"0"/garbage) keeps MCP enabled.
func TestDisableClaudeMCPFromEnv(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " On "} {
		if !DisableClaudeMCPFromEnv(v) {
			t.Errorf("DisableClaudeMCPFromEnv(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "nope", "2"} {
		if DisableClaudeMCPFromEnv(v) {
			t.Errorf("DisableClaudeMCPFromEnv(%q) = true, want false", v)
		}
	}
}

// TestBuildEnvAgentTmpDir asserts that a supplied AgentTmpDir is delivered as
// CLAUDE_CODE_TMPDIR (srt's sanctioned lever for the child's TMPDIR) and that
// rein never sets TMPDIR directly (srt owns it and would clobber a rein value).
func TestBuildEnvAgentTmpDir(t *testing.T) {
	env := BuildEnv(EnvParams{
		Parent:       []string{"HOME=/home/dev", "TMPDIR=/tmp/parent-should-not-win"},
		CABundlePath: "/run/ca-bundle.pem",
		StubGHToken:  "stub-tok",
		AgentTmpDir:  "/run/rein/agent-tmp",
	})
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["CLAUDE_CODE_TMPDIR"] != "/run/rein/agent-tmp" {
		t.Errorf("CLAUDE_CODE_TMPDIR = %q, want the agent scratch dir", got["CLAUDE_CODE_TMPDIR"])
	}
	// rein must NOT emit TMPDIR itself (srt sets it from CLAUDE_CODE_TMPDIR).
	if _, ok := got["TMPDIR"]; ok {
		t.Errorf("TMPDIR must not be set by rein (srt owns it); got %q", got["TMPDIR"])
	}
}

// TestBuildEnvGitIdentity asserts the four GIT_*_NAME/EMAIL vars and
// GIT_CONFIG_GLOBAL are set to exactly the resolved identity when supplied — the
// unit-level guarantee that a sandboxed commit resolves rein's non-impersonating
// identity, not the developer's (the live author check needs a real push, per
// the manual script).
func TestBuildEnvGitIdentity(t *testing.T) {
	env := BuildEnv(EnvParams{
		Parent:              []string{"HOME=/home/dev"},
		CABundlePath:        "/run/ca-bundle.pem",
		StubGHToken:         "stub-tok",
		GitAuthorName:       "Tom Hennen (via rein)",
		GitAuthorEmail:      "287259336+agentcreds-validation-beef[bot]@users.noreply.github.com",
		GitConfigGlobalPath: "/run/rein/gitconfig",
	})
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}

	wantName := "Tom Hennen (via rein)"
	wantEmail := "287259336+agentcreds-validation-beef[bot]@users.noreply.github.com"
	for _, tc := range []struct{ k, want string }{
		{"GIT_AUTHOR_NAME", wantName},
		{"GIT_COMMITTER_NAME", wantName},
		{"GIT_AUTHOR_EMAIL", wantEmail},
		{"GIT_COMMITTER_EMAIL", wantEmail},
		{"GIT_CONFIG_GLOBAL", "/run/rein/gitconfig"},
		{"GIT_CONFIG_SYSTEM", "/dev/null"},
	} {
		if got[tc.k] != tc.want {
			t.Errorf("%s = %q, want %q", tc.k, got[tc.k], tc.want)
		}
	}
	// The dev's real email must NEVER appear anywhere in the sandbox env.
	for _, kv := range env {
		if strings.Contains(kv, "tom.hennen@gmail.com") {
			t.Errorf("developer email leaked into sandbox env: %q", kv)
		}
	}
}

// TestBuildEnvCarriesWorktreeChannel (#64): the agent cannot guess where the
// developer's checkout of repo B lives on disk — rein must TELL it. The banner
// reaches the human; REIN_REPO_WORKTREES (JSON) and REIN_EPHEMERAL_CLONE_DIR
// reach the agent. Both are non-secret facts about the sandbox's own
// filesystem, and both are omitted entirely when there is nothing to say.
func TestBuildEnvCarriesWorktreeChannel(t *testing.T) {
	env := BuildEnv(EnvParams{
		Parent:            []string{"PATH=/usr/bin"},
		CABundlePath:      "/tmp/ca.pem",
		StubGHToken:       "stub",
		RepoWorktrees:     `[{"repo":"owner/b","path":"/srv/dev/b","mode":"rw"}]`,
		EphemeralCloneDir: "/tmp/rein-agent-tmp-1",
	})
	got := envMap(env)
	if got["REIN_REPO_WORKTREES"] != `[{"repo":"owner/b","path":"/srv/dev/b","mode":"rw"}]` {
		t.Errorf("REIN_REPO_WORKTREES = %q", got["REIN_REPO_WORKTREES"])
	}
	if got["REIN_EPHEMERAL_CLONE_DIR"] != "/tmp/rein-agent-tmp-1" {
		t.Errorf("REIN_EPHEMERAL_CLONE_DIR = %q", got["REIN_EPHEMERAL_CLONE_DIR"])
	}

	// Nothing mapped, nothing to clone into => the vars are absent, not empty
	// (an empty REIN_REPO_WORKTREES would read as "[] is authoritative" noise).
	got = envMap(BuildEnv(EnvParams{Parent: []string{"PATH=/usr/bin"}, CABundlePath: "/tmp/ca.pem", StubGHToken: "stub"}))
	if _, ok := got["REIN_REPO_WORKTREES"]; ok {
		t.Error("REIN_REPO_WORKTREES must be unset when no checkout is bound")
	}
	if _, ok := got["REIN_EPHEMERAL_CLONE_DIR"]; ok {
		t.Error("REIN_EPHEMERAL_CLONE_DIR must be unset when there is no clone dir")
	}
}

// envMap turns a BuildEnv result into a name->value map.
func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if ok {
			out[name] = value
		}
	}
	return out
}

// TestBuildEnvAgentVisibleFacts asserts the #63 agent-visible channel: the
// launch banner that explains the ephemeral $HOME goes to the HUMAN's terminal
// and the agent never sees it, so BuildEnv carries the two load-bearing facts
// INTO the sandbox — where the work persists (REIN_IN_SANDBOX_WORKTREE) and that $HOME
// does not (REIN_IN_SANDBOX_HOME=ephemeral), plus REIN_IN_SANDBOX=1 as the plain
// "you are inside rein" primitive for hooks and wrapper scripts.
//
// Direction is the thing to keep straight: these are WRITTEN BY rein for the
// agent, unlike REIN_SANDBOX_SHOW_HOME / REIN_SANDBOX_ALLOW_READ, which rein
// READS from its own env outside the sandbox. None of them carry a secret.
func TestBuildEnvAgentVisibleFacts(t *testing.T) {
	env := BuildEnv(EnvParams{
		Parent:        []string{"HOME=/home/dev"},
		CABundlePath:  "/run/ca-bundle.pem",
		StubGHToken:   "stub-tok",
		WorkTree:      "/work/repo",
		HomeEphemeral: true,
	})
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for _, tc := range []struct{ k, want string }{
		{"REIN_IN_SANDBOX", "1"},
		{"REIN_IN_SANDBOX_WORKTREE", "/work/repo"},
		{"REIN_IN_SANDBOX_HOME", "ephemeral"},
	} {
		if got[tc.k] != tc.want {
			t.Errorf("%s = %q, want %q", tc.k, got[tc.k], tc.want)
		}
	}

	// Kill-switch arm (REIN_SANDBOX_SHOW_HOME=1 upstream => HomeEphemeral false):
	// $HOME is a real, persistent home in-sandbox, so claiming it is ephemeral
	// would be a LIE to the agent. The var must be absent, not "false".
	env = BuildEnv(EnvParams{
		Parent:       []string{"HOME=/home/dev"},
		CABundlePath: "/run/ca-bundle.pem",
		StubGHToken:  "stub-tok",
		WorkTree:     "/work/repo",
	})
	for _, kv := range env {
		if strings.HasPrefix(kv, "REIN_IN_SANDBOX_HOME=") {
			t.Errorf("REIN_IN_SANDBOX_HOME must be absent when $HOME is NOT ephemeral; got %q", kv)
		}
	}
}

func TestBuildEnv_ExtraPathDirPrepended(t *testing.T) {
	env := BuildEnv(EnvParams{
		Parent:       []string{"PATH=/usr/bin:/bin", "HOME=/home/x"},
		CABundlePath: "/tmp/bundle.pem",
		StubGHToken:  "stub",
		ExtraPathDir: "/tmp/rein-run",
	})
	var path string
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			path = kv
		}
	}
	if path != "PATH=/tmp/rein-run:/usr/bin:/bin" {
		t.Errorf("PATH = %q, want the staged dir prepended (rein declare must resolve in-sandbox)", path)
	}
	// Without ExtraPathDir the PATH passes through untouched.
	env = BuildEnv(EnvParams{Parent: []string{"PATH=/usr/bin"}, CABundlePath: "b", StubGHToken: "s"})
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") && kv != "PATH=/usr/bin" {
			t.Errorf("PATH = %q, want untouched passthrough", kv)
		}
	}
}
