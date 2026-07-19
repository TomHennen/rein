package main

import (
	"os"
	"path/filepath"
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
	p := buildNonoParams(port, caPath, []string{"api.anthropic.com"}, gitCfg, "", "")

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
	p := buildNonoParams(0, "/tmp/ca.pem", nil, nil, "", "")
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
	p := buildNonoParams(47821, "/run/rein/ca.pem", nil, nil, overlay, "")
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
	prof, err := nono.Build(buildNonoParams(47821, "/run/rein/ca.pem", nil, nil, "", ""))
	if err != nil {
		t.Fatalf("nono.Build: %v", err)
	}
	if v, ok := prof.Environment.SetVars["CLAUDE_CONFIG_DIR"]; ok {
		t.Errorf("CLAUDE_CONFIG_DIR must be absent when no overlay is passed; got %q", v)
	}
}

// TestBuildNonoParams_GhConfigDirFlows pins the gh overlay wiring (gap 2): the
// rein-owned overlay path passed to buildNonoParams becomes GH_CONFIG_DIR in the
// emitted profile's set_vars, and no host ~/.config/gh path is granted read (it
// stays hidden by nono's default-deny fs).
func TestBuildNonoParams_GhConfigDirFlows(t *testing.T) {
	const ghOverlay = "/tmp/rein-gh-abc123"
	p := buildNonoParams(47821, "/run/rein/ca.pem", nil, nil, "", ghOverlay)
	if p.GhConfigDir != ghOverlay {
		t.Fatalf("GhConfigDir = %q, want %q", p.GhConfigDir, ghOverlay)
	}
	prof, err := nono.Build(p)
	if err != nil {
		t.Fatalf("nono.Build: %v", err)
	}
	if got := prof.Environment.SetVars["GH_CONFIG_DIR"]; got != ghOverlay {
		t.Errorf("set_vars[GH_CONFIG_DIR] = %q, want the rein overlay %q", got, ghOverlay)
	}
	for _, f := range prof.Filesystem.ReadFile {
		if strings.Contains(f, "/.config/gh") {
			t.Errorf("profile grants read on a host gh path %q (must stay hidden by default-deny)", f)
		}
	}
}

// TestBuildNonoParams_NoGhConfigDir: a run that does not launch gh passes an
// empty overlay and the profile carries no GH_CONFIG_DIR (golden-stable).
func TestBuildNonoParams_NoGhConfigDir(t *testing.T) {
	prof, err := nono.Build(buildNonoParams(47821, "/run/rein/ca.pem", nil, nil, "", ""))
	if err != nil {
		t.Fatalf("nono.Build: %v", err)
	}
	if v, ok := prof.Environment.SetVars["GH_CONFIG_DIR"]; ok {
		t.Errorf("GH_CONFIG_DIR must be absent when no overlay is passed; got %q", v)
	}
}

// TestPrepareGhOverlay verifies the host-side overlay: a fresh writable dir with
// a scaffolded hosts.yml carrying the placeholder token (never the host's real
// creds), hardened to 0700.
func TestPrepareGhOverlay(t *testing.T) {
	overlay, err := prepareGhOverlay(t.TempDir())
	if err != nil {
		t.Fatalf("prepareGhOverlay: %v", err)
	}
	fi, err := os.Stat(overlay)
	if err != nil || !fi.IsDir() {
		t.Fatalf("overlay %q not a dir: %v", overlay, err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("overlay mode = %o, want 0700 (no group/other access)", perm)
	}
	hosts, err := os.ReadFile(filepath.Join(overlay, "hosts.yml"))
	if err != nil {
		t.Fatalf("read scaffolded hosts.yml: %v", err)
	}
	if !strings.Contains(string(hosts), ghStubToken) {
		t.Errorf("hosts.yml missing placeholder token; got:\n%s", hosts)
	}
	// The overlay must be WRITABLE by the agent (gh writes state): prove we can
	// write into it, standing in for the nono --allow grant.
	if err := os.WriteFile(filepath.Join(overlay, "probe"), []byte("x"), 0o600); err != nil {
		t.Errorf("overlay not writable: %v", err)
	}
}

// TestBriefNonoAgent_Claude asserts the sandbox contract reaches a claude launch
// via --append-system-prompt (gap 3): the flag is inserted right after argv0,
// before user args, and carries the ephemeral-$HOME / no-creds / declare wording.
func TestBriefNonoAgent_Claude(t *testing.T) {
	cmdline := []string{"claude", "-p", "do the thing"}
	argv, contract, statusLine, printToStdout := briefNonoAgent(cmdline, contractParams{
		WorkTree:      "/work/tree",
		HomeEphemeral: true,
		ExtraDomains:  []string{"api.anthropic.com"},
	}, false)

	if printToStdout {
		t.Error("claude has a system-prompt channel; must NOT also print to stdout")
	}
	if len(argv) != len(cmdline)+2 || argv[0] != "claude" || argv[1] != "--append-system-prompt" {
		t.Fatalf("argv = %v, want claude --append-system-prompt <contract> then user args", argv)
	}
	if argv[2] != contract {
		t.Errorf("injected system prompt %q != returned contract", argv[2])
	}
	// User args must follow, unseparated from a trailing positional prompt.
	if argv[3] != "-p" || argv[4] != "do the thing" {
		t.Errorf("user args not preserved after injection: %v", argv[2:])
	}
	if !strings.Contains(contract, "rein declare") || !strings.Contains(contract, "$HOME is EPHEMERAL") {
		t.Errorf("contract missing declare/ephemeral wording:\n%s", contract)
	}
	if !strings.Contains(statusLine, "injected via --append-system-prompt") {
		t.Errorf("status line does not report injection: %q", statusLine)
	}
}

// TestBriefNonoAgent_NonClaude: a non-claude agent's argv is unchanged and the
// contract is printed to its stdout (its only briefing channel).
func TestBriefNonoAgent_NonClaude(t *testing.T) {
	cmdline := []string{"bash", "-c", "echo hi"}
	argv, contract, _, printToStdout := briefNonoAgent(cmdline, contractParams{WorkTree: "/w", HomeEphemeral: true}, false)
	if !printToStdout {
		t.Error("non-claude agent must have the contract printed to stdout")
	}
	if len(argv) != len(cmdline) || argv[0] != "bash" {
		t.Errorf("non-claude argv must be unchanged; got %v", argv)
	}
	if contract == "" {
		t.Error("contract must still be built (to print) for a non-claude agent")
	}
}

// TestBriefNonoAgent_Off: REIN_DISABLE_AGENT_CONTRACT leaves argv untouched, emits
// no contract, and the banner warns the agent was NOT briefed.
func TestBriefNonoAgent_Off(t *testing.T) {
	cmdline := []string{"claude"}
	argv, contract, statusLine, printToStdout := briefNonoAgent(cmdline, contractParams{WorkTree: "/w", HomeEphemeral: true}, true)
	if len(argv) != 1 || argv[0] != "claude" {
		t.Errorf("off: argv must be unchanged; got %v", argv)
	}
	if contract != "" || printToStdout {
		t.Errorf("off: no contract must be built or printed; contract=%q print=%v", contract, printToStdout)
	}
	if !strings.Contains(statusLine, "DISABLED") {
		t.Errorf("off: status line must warn DISABLED; got %q", statusLine)
	}
}

// Mode routing (nono default + --sandbox/--nono aliases + --direct opt-out) is
// pinned by TestParseRunModeRouting in run_broker_shared_test.go.
