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

	// ENABLE_CLAUDEAI_MCP_SERVERS is set unconditionally to the string "false"
	// (must be "false", not "0"), disabling claude.ai remote MCP connectors so
	// startup does not hang on hosts outside the egress allowlist.
	if got["ENABLE_CLAUDEAI_MCP_SERVERS"] != "false" {
		t.Errorf("ENABLE_CLAUDEAI_MCP_SERVERS = %q, want \"false\"", got["ENABLE_CLAUDEAI_MCP_SERVERS"])
	}

	// No AgentTmpDir supplied here -> CLAUDE_CODE_TMPDIR must be absent (the probe
	// path does no temp work). It is present only when a scratch dir is provided
	// (asserted in TestBuildEnvAgentTmpDir).
	if _, ok := got["CLAUDE_CODE_TMPDIR"]; ok {
		t.Errorf("CLAUDE_CODE_TMPDIR present with no AgentTmpDir: %q", got["CLAUDE_CODE_TMPDIR"])
	}

	// The full set is ONLY passthrough + CA vars + GH_TOKEN + GIT_CONFIG_SYSTEM +
	// ENABLE_CLAUDEAI_MCP_SERVERS (no identity or AgentTmpDir supplied here) —
	// nothing else.
	allowed := map[string]bool{"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "TERM": true, "GH_TOKEN": true, "GIT_CONFIG_SYSTEM": true, "ENABLE_CLAUDEAI_MCP_SERVERS": true}
	for _, name := range caEnvVars {
		allowed[name] = true
	}
	for k := range got {
		if !allowed[k] {
			t.Errorf("unexpected env var %q in sandbox env", k)
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
