package srt

import (
	"strings"
	"testing"
)

// TestBuildExternalProxyShape pins the #185 network stanza: both proxy ports
// = rein's port, no mitmProxy, a DEFINED but empty allowlist (srt keeps the
// namespace unshared yet has nothing to allow), deny-all, strictAllowlist;
// filesystem rules unchanged. And the shape validation rejects mixes.
func TestBuildExternalProxyShape(t *testing.T) {
	cfg, err := Build(Params{
		SocketPath:          "/run/user/1000/rein/run-x/proxy.sock",
		WorkingTree:         "/home/dev/work/repo",
		ExternalProxyPort:   40123,
		ExtraAllowedDomains: []string{"pypi.org"}, // ignored here: rein's policy owns it
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	n := cfg.Network
	if n.HttpProxyPort == nil || n.SocksProxyPort == nil || *n.HttpProxyPort != 40123 || *n.SocksProxyPort != 40123 {
		t.Errorf("proxy ports = %v/%v, want both 40123", n.HttpProxyPort, n.SocksProxyPort)
	}
	if n.MitmProxy != nil {
		t.Error("mitmProxy must be absent in the external shape")
	}
	if n.AllowedDomains == nil || len(n.AllowedDomains) != 0 {
		t.Errorf("allowedDomains = %#v, want a defined empty array", n.AllowedDomains)
	}
	if len(n.DeniedDomains) != 1 || n.DeniedDomains[0] != "*" {
		t.Errorf("deniedDomains = %v, want [*]", n.DeniedDomains)
	}
	data, _ := cfg.MarshalIndent()
	if !strings.Contains(string(data), `"allowedDomains": []`) {
		t.Errorf("allowedDomains must serialize as [] (srt's hasNetworkConfig is `!== undefined`):\n%s", data)
	}
	if strings.Contains(string(data), "mitmProxy") {
		t.Errorf("mitmProxy leaked into the external-shape JSON:\n%s", data)
	}

	port, other := 40123, 40124
	shape := func(n Network) Config { return Config{Network: n, Filesystem: cfg.Filesystem} }
	bad := map[string]Config{
		"no socks port":       shape(Network{StrictAllowlist: true, HttpProxyPort: &port, AllowedDomains: []string{}, DeniedDomains: []string{"*"}}),
		"different ports":     shape(Network{StrictAllowlist: true, HttpProxyPort: &port, SocksProxyPort: &other, AllowedDomains: []string{}, DeniedDomains: []string{"*"}}),
		"non-empty allowlist": shape(Network{StrictAllowlist: true, HttpProxyPort: &port, SocksProxyPort: &port, AllowedDomains: []string{"pypi.org"}, DeniedDomains: []string{"*"}}),
		"no deny-all":         shape(Network{StrictAllowlist: true, HttpProxyPort: &port, SocksProxyPort: &port, AllowedDomains: []string{}, DeniedDomains: []string{}}),
		"mixed with mitm":     shape(Network{StrictAllowlist: true, HttpProxyPort: &port, SocksProxyPort: &port, AllowedDomains: []string{}, DeniedDomains: []string{"*"}, MitmProxy: &MitmProxy{SocketPath: "/x"}}),
		"nil allowlist":       shape(Network{StrictAllowlist: true, HttpProxyPort: &port, SocksProxyPort: &port, DeniedDomains: []string{"*"}}),
		"not strict":          shape(Network{StrictAllowlist: false, HttpProxyPort: &port, SocksProxyPort: &port, AllowedDomains: []string{}, DeniedDomains: []string{"*"}}),
	}
	for name, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("shape %q validated", name)
		}
	}
	if _, err := Build(Params{SocketPath: "/s/proxy.sock", WorkingTree: "/w", ExternalProxyPort: 70000}); err == nil {
		t.Error("out-of-range external port accepted")
	}
}
