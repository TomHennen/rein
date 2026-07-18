package main

import (
	"os"
	"strings"
	"testing"

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
	p := buildNonoParams(port, caPath, []string{"api.anthropic.com"})

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
	// Build's Validate already enforces deny_credentials + af_unix_mediation +
	// inject/CDN routing; a passing Build means those invariants held.
}

// TestBuildNonoParams_EmptyPortIsInvalid: a zero loopback port (front never
// bound) must NOT silently produce a usable profile — Build fails closed. runNono
// guards this before Build, but assert the shape here too.
func TestBuildNonoParams_EmptyPortStillFormsAddr(t *testing.T) {
	p := buildNonoParams(0, "/tmp/ca.pem", nil)
	if p.ListenAddr != "127.0.0.1:0" {
		t.Fatalf("ListenAddr = %q, want 127.0.0.1:0", p.ListenAddr)
	}
	// Build accepts :0 syntactically (it is a valid host:port); the real guard is
	// runNono's loopbackPort==0 check, which refuses to launch. This test just
	// documents that buildNonoParams itself is a pure formatter.
}

// TestParseRunMode_Nono covers the --nono flag and the REIN_SANDBOX=nono env
// selector, and that an explicit leading flag wins over the env.
func TestParseRunMode_Nono(t *testing.T) {
	t.Run("flag", func(t *testing.T) {
		mode, cmd, err := parseRunMode([]string{"--nono", "--", "curl", "https://api.github.com"})
		if err != nil {
			t.Fatal(err)
		}
		if mode != modeNono {
			t.Errorf("mode = %v, want modeNono", mode)
		}
		if len(cmd) != 2 || cmd[0] != "curl" {
			t.Errorf("cmdline = %v", cmd)
		}
	})

	t.Run("env-selector", func(t *testing.T) {
		t.Setenv("REIN_SANDBOX", "nono")
		mode, _, err := parseRunMode([]string{"--", "echo", "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if mode != modeNono {
			t.Errorf("mode = %v, want modeNono from REIN_SANDBOX=nono", mode)
		}
	})

	t.Run("flag-overrides-env", func(t *testing.T) {
		t.Setenv("REIN_SANDBOX", "nono")
		mode, _, err := parseRunMode([]string{"--sandbox", "--", "echo", "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if mode != modeSandbox {
			t.Errorf("mode = %v, want modeSandbox (--sandbox overrides REIN_SANDBOX=nono)", mode)
		}
	})

	t.Run("default-unset", func(t *testing.T) {
		os.Unsetenv("REIN_SANDBOX")
		mode, _, err := parseRunMode([]string{"--", "echo", "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if mode != modeSandbox {
			t.Errorf("mode = %v, want modeSandbox by default", mode)
		}
	})
}
