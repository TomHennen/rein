package srt

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildGoldenShape(t *testing.T) {
	cfg, err := Build(Params{
		SocketPath:  "/run/user/1000/rein/run-x/proxy.sock",
		WorkingTree: "/home/dev/work/repo",
		DenyReadCredStores: []string{
			"/home/dev/.config/gh",
			"/home/dev/.ssh",
			"/home/dev/.netrc",
		},
		RuntimeDenyRead: []string{"/run/user/1000/rein/run-x", "/run/user/1000"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got, err := cfg.MarshalIndent()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Round-trip and assert the load-bearing invariants rather than a brittle
	// byte-golden (denyRead order is sorted; a golden would churn on any path).
	var rt Config
	if err := json.Unmarshal(got, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// allowedDomains = exactly the 3 inject + 3 CDN hosts + the local
	// declare virtual host (issue #35).
	wantAllowed := map[string]bool{
		"github.com": true, "api.github.com": true, "uploads.github.com": true,
		"codeload.github.com": true, "objects.githubusercontent.com": true, "raw.githubusercontent.com": true,
		"declare.rein.internal": true,
	}
	if len(rt.Network.AllowedDomains) != len(wantAllowed) {
		t.Errorf("allowedDomains = %v, want 7 (3 inject + 3 cdn + declare host)", rt.Network.AllowedDomains)
	}
	for _, d := range rt.Network.AllowedDomains {
		if !wantAllowed[d] {
			t.Errorf("unexpected allowed domain %q", d)
		}
	}

	// mitmProxy.domains = EXACTLY the 3 inject hosts + the local declare
	// host (routed to the socket, answered locally) — no CDN, no wildcard.
	wantInject := map[string]bool{
		"github.com": true, "api.github.com": true, "uploads.github.com": true,
		"declare.rein.internal": true,
	}
	if rt.Network.MitmProxy == nil {
		t.Fatal("mitmProxy nil")
	}
	if len(rt.Network.MitmProxy.Domains) != 4 {
		t.Errorf("mitmProxy.domains = %v, want the 3 inject hosts + declare host", rt.Network.MitmProxy.Domains)
	}
	for _, d := range rt.Network.MitmProxy.Domains {
		if !wantInject[d] {
			t.Errorf("mitmProxy.domains includes non-inject host %q", d)
		}
		if strings.Contains(d, "*") {
			t.Errorf("mitmProxy.domains has wildcard %q", d)
		}
	}
	if rt.Network.MitmProxy.SocketPath != "/run/user/1000/rein/run-x/proxy.sock" {
		t.Errorf("socketPath = %q", rt.Network.MitmProxy.SocketPath)
	}

	// strictAllowlist true; deniedDomains empty (present, not null).
	if !rt.Network.StrictAllowlist {
		t.Error("strictAllowlist must be true")
	}
	if rt.Network.DeniedDomains == nil {
		t.Error("deniedDomains must marshal as [] not null")
	}

	// CDN hosts are allowed egress but NOT injected.
	for _, cdn := range []string{"codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com"} {
		for _, d := range rt.Network.MitmProxy.Domains {
			if d == cdn {
				t.Errorf("CDN host %q must not be injected", cdn)
			}
		}
	}

	// filesystem: working tree in allowWrite (not allowRead); denyRead covers
	// the credential stores; allowRead/denyWrite empty but present.
	if len(rt.Filesystem.AllowWrite) != 1 || rt.Filesystem.AllowWrite[0] != "/home/dev/work/repo" {
		t.Errorf("allowWrite = %v, want [working tree]", rt.Filesystem.AllowWrite)
	}
	if len(rt.Filesystem.AllowRead) != 0 {
		t.Errorf("allowRead must be empty (working tree goes in allowWrite), got %v", rt.Filesystem.AllowRead)
	}
	drSet := map[string]bool{}
	for _, d := range rt.Filesystem.DenyRead {
		drSet[d] = true
	}
	for _, want := range []string{"/home/dev/.config/gh", "/home/dev/.ssh", "/home/dev/.netrc", "/run/user/1000"} {
		if !drSet[want] {
			t.Errorf("denyRead missing %q; got %v", want, rt.Filesystem.DenyRead)
		}
	}
}

// TestBuildExtraAllowWrite asserts that an ExtraAllowWrite dir (the per-run agent
// scratch dir that becomes the sandboxed child's TMPDIR) is bound writable
// ALONGSIDE the working tree — the config-level half of the EROFS fix.
func TestBuildExtraAllowWrite(t *testing.T) {
	rt, err := Build(Params{
		SocketPath:      "/run/user/1000/rein/run-x/proxy.sock",
		WorkingTree:     "/home/dev/work/repo",
		ExtraAllowWrite: []string{"/tmp/rein-agent-tmp-abc"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	awSet := map[string]bool{}
	for _, w := range rt.Filesystem.AllowWrite {
		awSet[w] = true
	}
	for _, want := range []string{"/home/dev/work/repo", "/tmp/rein-agent-tmp-abc"} {
		if !awSet[want] {
			t.Errorf("allowWrite missing %q; got %v", want, rt.Filesystem.AllowWrite)
		}
	}
}

// TestBuildThreadsExtraDomainsEgressOnly asserts CP4.5's core invariant: extra
// egress domains land in allowedDomains (egress-allowed) but NEVER in
// mitmProxy.domains (never injected), and a duplicate/GitHub host dedupes.
func TestBuildThreadsExtraDomainsEgressOnly(t *testing.T) {
	cfg, err := Build(Params{
		SocketPath:  "/run/user/1000/rein/run-x/proxy.sock",
		WorkingTree: "/home/dev/work/repo",
		ExtraAllowedDomains: []string{
			"api.anthropic.com",
			"registry.npmjs.org",
			"*.internal.example.com",
			"github.com", // duplicate of an inject host -> must dedupe, not double-list
		},
	})
	if err != nil {
		t.Fatalf("Build with extra domains: %v", err)
	}

	// Each extra host is egress-allowed.
	for _, want := range []string{"api.anthropic.com", "registry.npmjs.org", "*.internal.example.com"} {
		if !contains(cfg.Network.AllowedDomains, want) {
			t.Errorf("extra domain %q missing from allowedDomains: %v", want, cfg.Network.AllowedDomains)
		}
	}
	// github.com appears exactly once (deduped against the inject host).
	var ghCount int
	for _, d := range cfg.Network.AllowedDomains {
		if d == "github.com" {
			ghCount++
		}
	}
	if ghCount != 1 {
		t.Errorf("github.com should appear once in allowedDomains, got %d: %v", ghCount, cfg.Network.AllowedDomains)
	}

	// NONE of the extra hosts may be injected — mitmProxy.domains stays EXACTLY
	// the three GitHub inject hosts + the local declare host.
	if len(cfg.Network.MitmProxy.Domains) != 4 {
		t.Fatalf("mitmProxy.domains must stay the 3 inject hosts + declare host, got %v", cfg.Network.MitmProxy.Domains)
	}
	for _, d := range cfg.Network.MitmProxy.Domains {
		if d == "api.anthropic.com" || d == "registry.npmjs.org" || strings.Contains(d, "*") {
			t.Errorf("extra/egress host %q leaked into mitmProxy.domains (would be injected!)", d)
		}
	}
}

// TestValidateRejectsAllowAllWildcard: a bare `*` (or empty) in allowedDomains
// defeats strictAllowlist and must be rejected; a strict *.suffix is fine.
func TestValidateRejectsAllowAllWildcard(t *testing.T) {
	good, err := Build(Params{SocketPath: "/run/s.sock", WorkingTree: "/w"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// *.suffix is accepted.
	okWild := deepCopy(t, good)
	okWild.Network.AllowedDomains = append(okWild.Network.AllowedDomains, "*.anthropic.com")
	if err := okWild.Validate(); err != nil {
		t.Errorf("Validate rejected a legal *.suffix in allowedDomains: %v", err)
	}
	// bare * / empty / wildcards covering a managed GitHub host are rejected.
	for _, bad := range []string{"*", "", "*.*", "*.github.com", "*.githubusercontent.com"} {
		c := deepCopy(t, good)
		c.Network.AllowedDomains = append(c.Network.AllowedDomains, bad)
		if err := c.Validate(); err == nil {
			t.Errorf("Validate accepted conflicting/over-broad allowedDomains entry %q", bad)
		}
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	if _, err := Build(Params{WorkingTree: "/x"}); err == nil {
		t.Error("Build allowed empty SocketPath")
	}
	if _, err := Build(Params{SocketPath: "/x/s.sock"}); err == nil {
		t.Error("Build allowed empty WorkingTree")
	}
}

func TestValidateCatchesWeakenings(t *testing.T) {
	good, err := Build(Params{SocketPath: "/run/s.sock", WorkingTree: "/w"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"strictAllowlist off", func(c *Config) { c.Network.StrictAllowlist = false }},
		{"nil mitmProxy", func(c *Config) { c.Network.MitmProxy = nil }},
		{"empty allowedDomains", func(c *Config) { c.Network.AllowedDomains = nil }},
		{"wildcard inject domain", func(c *Config) { c.Network.MitmProxy.Domains = []string{"*.github.com"} }},
		{"cdn in inject domains", func(c *Config) {
			c.Network.MitmProxy.Domains = append(c.Network.MitmProxy.Domains, "codeload.github.com")
			c.Network.AllowedDomains = append(c.Network.AllowedDomains, "codeload.github.com")
		}},
		{"empty allowWrite", func(c *Config) { c.Filesystem.AllowWrite = nil }},
		{"working tree under denyRead", func(c *Config) {
			c.Filesystem.DenyRead = append(c.Filesystem.DenyRead, "/w")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := deepCopy(t, good)
			tt.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("Validate accepted a weakened config (%s)", tt.name)
			}
		})
	}
}

func deepCopy(t *testing.T, c Config) Config {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
