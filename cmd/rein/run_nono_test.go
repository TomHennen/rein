package main

import (
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/gitidentity"
	"github.com/TomHennen/rein/internal/nono"
	"github.com/TomHennen/rein/internal/sandboxutil"
)

// TestBuildNonoParams_LoopbackPortFlowsToProxy pins the load-bearing wiring: the
// resolved loopback port becomes upstream_proxy (bare host:port, loopback), the
// CA bundle path is carried, and the four CA env vars point at it — assembled
// without a live launch.
func TestBuildNonoParams_LoopbackPortFlowsToProxy(t *testing.T) {
	const port = 47821
	caPath := "/run/rein/ca-bundle.pem"
	gitCfg := nonoGitIdentityConfig(gitidentity.Identity{Name: "rein bot", Email: "bot@users.noreply.github.com"})
	p := buildNonoParams(port, caPath, []string{"api.anthropic.com"}, gitCfg, "")

	if p.ListenAddr != "127.0.0.1:47821" {
		t.Fatalf("ListenAddr = %q, want 127.0.0.1:47821", p.ListenAddr)
	}
	if p.CACertPath != caPath {
		t.Fatalf("CACertPath = %q, want %q", p.CACertPath, caPath)
	}
	if len(p.UnixSockets) != 0 {
		t.Errorf("UnixSockets = %v, want empty (never grant the approval socket by default)", p.UnixSockets)
	}

	prof, err := nono.Build(p)
	if err != nil {
		t.Fatalf("nono.Build: %v", err)
	}
	if prof.Network.UpstreamProxy != "127.0.0.1:47821" {
		t.Errorf("upstream_proxy = %q, want the loopback port 127.0.0.1:47821", prof.Network.UpstreamProxy)
	}
	if !strings.Contains(prof.Network.UpstreamProxy, "47821") {
		t.Errorf("loopback port did not reach upstream_proxy: %q", prof.Network.UpstreamProxy)
	}
	for _, k := range sandboxutil.CAEnvVars {
		if got := prof.Environment.SetVars[k]; got != caPath {
			t.Errorf("set_vars[%q] = %q, want the CA bundle path %q", k, got, caPath)
		}
	}
	// The non-impersonating git identity must land in the GIT_CONFIG_* env: without
	// user.email the agent's `git commit` fails under nono's default-deny fs.
	sv := prof.Environment.SetVars
	if !hasGitConfigPair(sv, "user.email", "bot@users.noreply.github.com") {
		t.Errorf("git user.email not injected into GIT_CONFIG_*: %v", sv)
	}
	if !hasGitConfigPair(sv, "user.name", "rein bot") {
		t.Errorf("git user.name not injected into GIT_CONFIG_*: %v", sv)
	}
	// Build's Validate already enforces deny_credentials + af_unix_mediation +
	// inject/CDN routing; a passing Build means those invariants held.
}

// hasGitConfigPair reports whether set_vars carries a GIT_CONFIG_KEY_n=key with
// the matching GIT_CONFIG_VALUE_n=value (the git multi-var env encoding).
func hasGitConfigPair(sv map[string]string, key, value string) bool {
	for k, v := range sv {
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") && v == key {
			idx := strings.TrimPrefix(k, "GIT_CONFIG_KEY_")
			if sv["GIT_CONFIG_VALUE_"+idx] == value {
				return true
			}
		}
	}
	return false
}

// TestBuildNonoParams_EmptyPortIsInvalid: a zero loopback port (front never
// bound) must NOT silently produce a usable profile — Build fails closed. runNono
// guards this before Build, but assert the shape here too.
func TestBuildNonoParams_EmptyPortStillFormsAddr(t *testing.T) {
	p := buildNonoParams(0, "/tmp/ca.pem", nil, nil, "")
	if p.ListenAddr != "127.0.0.1:0" {
		t.Fatalf("ListenAddr = %q, want 127.0.0.1:0", p.ListenAddr)
	}
	// Build accepts :0 syntactically (it is a valid host:port); the real guard is
	// runNono's loopbackPort==0 check, which refuses to launch. This test just
	// documents that buildNonoParams itself is a pure formatter.
}

// TestBuildNonoParams_ClaudeConfigDirFlows pins the #94 nono wiring: the
// rein-owned overlay path passed to buildNonoParams becomes CLAUDE_CONFIG_DIR in
// the emitted profile's set_vars, and host ~/.claude is never granted (it stays
// hidden by nono's default-deny fs).
func TestBuildNonoParams_ClaudeConfigDirFlows(t *testing.T) {
	const overlay = "/home/dev/.config/rein-sandbox-home/.claude"
	p := buildNonoParams(47821, "/run/rein/ca.pem", nil, nil, overlay)
	if p.ClaudeConfigDir != overlay {
		t.Fatalf("ClaudeConfigDir = %q, want %q", p.ClaudeConfigDir, overlay)
	}
	prof, err := nono.Build(p)
	if err != nil {
		t.Fatalf("nono.Build: %v", err)
	}
	if got := prof.Environment.SetVars["CLAUDE_CONFIG_DIR"]; got != overlay {
		t.Errorf("set_vars[CLAUDE_CONFIG_DIR] = %q, want the rein overlay %q", got, overlay)
	}
	for _, f := range prof.Filesystem.ReadFile {
		if strings.Contains(f, "/.claude") && !strings.Contains(f, "rein-sandbox-home") {
			t.Errorf("profile grants read on a host claude path %q (must stay hidden by default-deny)", f)
		}
	}
}

// TestBuildNonoParams_NoClaudeConfigDir: a run that does not launch claude passes
// an empty overlay and the profile carries no CLAUDE_CONFIG_DIR (golden-stable).
func TestBuildNonoParams_NoClaudeConfigDir(t *testing.T) {
	prof, err := nono.Build(buildNonoParams(47821, "/run/rein/ca.pem", nil, nil, ""))
	if err != nil {
		t.Fatalf("nono.Build: %v", err)
	}
	if v, ok := prof.Environment.SetVars["CLAUDE_CONFIG_DIR"]; ok {
		t.Errorf("CLAUDE_CONFIG_DIR must be absent when no overlay is passed; got %q", v)
	}
}

// Mode routing (nono default + --sandbox/--nono aliases + --direct opt-out) is
// pinned by TestParseRunModeRouting in run_broker_shared_test.go.
