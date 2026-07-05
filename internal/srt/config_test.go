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

	// allowedDomains = exactly the 3 inject + 3 CDN hosts.
	wantAllowed := map[string]bool{
		"github.com": true, "api.github.com": true, "uploads.github.com": true,
		"codeload.github.com": true, "objects.githubusercontent.com": true, "raw.githubusercontent.com": true,
	}
	if len(rt.Network.AllowedDomains) != len(wantAllowed) {
		t.Errorf("allowedDomains = %v, want 6 (3 inject + 3 cdn)", rt.Network.AllowedDomains)
	}
	for _, d := range rt.Network.AllowedDomains {
		if !wantAllowed[d] {
			t.Errorf("unexpected allowed domain %q", d)
		}
	}

	// mitmProxy.domains = EXACTLY the 3 inject hosts, no CDN, no wildcard.
	wantInject := map[string]bool{"github.com": true, "api.github.com": true, "uploads.github.com": true}
	if rt.Network.MitmProxy == nil {
		t.Fatal("mitmProxy nil")
	}
	if len(rt.Network.MitmProxy.Domains) != 3 {
		t.Errorf("mitmProxy.domains = %v, want exactly 3 inject hosts", rt.Network.MitmProxy.Domains)
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
