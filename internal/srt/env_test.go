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

	// The full set is ONLY passthrough + CA vars + GH_TOKEN — nothing else.
	allowed := map[string]bool{"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "TERM": true, "GH_TOKEN": true}
	for _, name := range caEnvVars {
		allowed[name] = true
	}
	for k := range got {
		if !allowed[k] {
			t.Errorf("unexpected env var %q in sandbox env", k)
		}
	}
}
