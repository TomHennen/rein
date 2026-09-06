package proxy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeResolver maps names to addresses; unknown names fail.
func fakeResolver(m map[string][]string) func(context.Context, string) ([]net.IPAddr, error) {
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
		addrs, ok := m[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		out := make([]net.IPAddr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, net.IPAddr{IP: net.ParseIP(a)})
		}
		return out, nil
	}
}

func newTestPolicy(t *testing.T, mode EgressMode, domains, internal []string) *EgressPolicy {
	t.Helper()
	p, err := NewEgressPolicy(mode, domains, internal, []int{4443})
	if err != nil {
		t.Fatal(err)
	}
	p.Resolve = fakeResolver(map[string][]string{
		"pypi.org":               {"151.101.0.223"},
		"files.pythonhosted.org": {"151.101.0.175"},
		"example.com":            {"93.184.216.34"},
		"evil.example":           {"93.184.216.34", "10.0.0.5"}, // one public, one private
		"internal.corp":          {"10.1.2.3"},
		"tailnet.corp":           {"100.64.0.1"},
		"metadata.corp":          {"169.254.169.254"},
		"loop.corp":              {"127.0.0.1"},
		"nat64.corp":             {"64:ff9b::7f00:1"},
		"sixtofour.corp":         {"2002:7f00:1::1"},
		"teredo.corp":            {"2001:0:1:2:3:4:80ff:fffe"}, // XOR ffffffff => 127.0.0.1
		"sitelocal.corp":         {"fec0::1"},
		"v6ok.example":           {"2606:2800:220:1:248:1893:25c8:1946"},
	})
	return p
}

// TestEgressDecide_NeverRouteIsUnconditional: every loopback/private/link-local
// form is refused in BOTH modes, and an allowlisted name resolving there is
// refused too (the allowlist never overrides never-route).
func TestEgressDecide_NeverRouteIsUnconditional(t *testing.T) {
	for _, mode := range []EgressMode{EgressRestricted, EgressOpen} {
		// Every name is LISTED so restricted mode resolves it (an unlisted name
		// is refused before any lookup; see TestEgressDecide_RestrictedNeverResolvesUnlisted).
		p := newTestPolicy(t, mode, []string{"loop.corp", "internal.corp", "metadata.corp", "nat64.corp", "sixtofour.corp", "teredo.corp", "sitelocal.corp", "tailnet.corp", "evil.example", "127.1", "0x7f000001"}, nil)
		cases := map[string]string{
			"localhost":          "loopback",
			"foo.localhost":      "loopback",
			"127.1":              "loopback", // resolves via fake? no: IP shorthand is NOT an IP literal to ParseIP -> resolve fails
			"127.0.0.1":          "loopback",
			"0.0.0.0":            "loopback",
			"[::1]":              "loopback",
			"[::ffff:127.0.0.1]": "loopback",
			"[::ffff:7f00:1]":    "loopback",
			"[64:ff9b::7f00:1]":  "loopback",
			"nat64.corp":         "loopback",
			"sixtofour.corp":     "loopback",
			"teredo.corp":        "loopback",
			"loop.corp":          "loopback",
			"[fe80::1]":          "link-local",
			"[fe80::1%25eth0]":   "syntax",
			"169.254.169.254":    "link-local",
			"metadata.corp":      "link-local",
			"sitelocal.corp":     "link-local",
			"10.0.0.1":           "private",
			"internal.corp":      "private",
			"tailnet.corp":       "private",
			"100.64.0.1":         "private",
			"[fd00::1]":          "private",
			"evil.example":       "private", // one private address poisons the name
			"240.0.0.1":          "reserved",
			"255.255.255.255":    "reserved",
		}
		for host, want := range cases {
			d := p.Decide(context.Background(), host, 443)
			if d.Allowed {
				t.Errorf("mode=%s host=%s ALLOWED; want refused-egress-%s", mode, host, want)
				continue
			}
			if want == "loopback" && host == "127.1" {
				// inet_aton shorthand is not an IP literal; it fails resolution
				// (refused-egress-resolve), which is also a refusal.
				continue
			}
			if !strings.HasSuffix(d.Reason, "-"+want) {
				t.Errorf("mode=%s host=%s reason=%s; want *-%s", mode, host, d.Reason, want)
			}
		}
	}
}

// TestEgressDecide_RestrictedNeverResolvesUnlisted: in restricted mode an
// unlisted name is refused WITHOUT a lookup (a resolve would be a DNS
// exfiltration channel: CONNECT <data>.attacker.tld). Listed names resolve.
func TestEgressDecide_RestrictedNeverResolvesUnlisted(t *testing.T) {
	p := newTestPolicy(t, EgressRestricted, []string{"pypi.org", "*.pythonhosted.org"}, []string{"internal.corp:8443"})
	inner := p.Resolve
	p.Resolve = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "pypi.org", "files.pythonhosted.org", "internal.corp", "codeload.github.com":
			return inner(ctx, host)
		}
		t.Errorf("resolver called for unlisted host %q", host)
		return nil, &net.DNSError{Err: "must not resolve", Name: host}
	}
	if d := p.Decide(context.Background(), "exfil-chunk.attacker.tld", 443); d.Allowed || d.Reason != "refused-egress-host" {
		t.Errorf("unlisted => %+v", d)
	}
	if d := p.Decide(context.Background(), "pypi.org", 443); !d.Allowed {
		t.Errorf("listed => %+v", d)
	}
	// CDN hosts are on the list by construction (raw tunnels in restricted mode).
	p.Resolve = fakeResolver(map[string][]string{"codeload.github.com": {"140.82.112.9"}})
	if d := p.Decide(context.Background(), "codeload.github.com", 443); !d.Allowed || len(d.Addrs) == 0 {
		t.Errorf("CDN host in restricted mode => %+v", d)
	}
}

// TestNewEgressPolicy_RejectsPublicSuffixWildcards: `*.com` (and friends) is
// open mode by another name; only a two-label suffix is a wildcard.
func TestNewEgressPolicy_RejectsPublicSuffixWildcards(t *testing.T) {
	for _, bad := range []string{"*.com", "*.org", "*.io", "*"} {
		if _, err := NewEgressPolicy(EgressRestricted, []string{bad}, nil, nil); err == nil {
			t.Errorf("wildcard %q accepted", bad)
		}
	}
	if _, err := NewEgressPolicy(EgressRestricted, []string{"*.example.com", "_dmarc.example.com"}, nil, nil); err != nil {
		t.Errorf("legitimate entries rejected: %v", err)
	}
}

// TestEgressDecide_Normalizes: uppercase and a trailing FQDN dot match the
// lowercase list entry; underscores are legal host characters.
func TestEgressDecide_Normalizes(t *testing.T) {
	p := newTestPolicy(t, EgressRestricted, []string{"pypi.org", "_dmarc.example"}, nil)
	p.Resolve = fakeResolver(map[string][]string{"pypi.org": {"151.101.0.223"}, "_dmarc.example": {"93.184.216.34"}})
	for _, host := range []string{"PyPI.org", "pypi.org."} {
		if d := p.Decide(context.Background(), host, 443); !d.Allowed {
			t.Errorf("%q => %+v", host, d)
		}
	}
	if d := p.Decide(context.Background(), "_dmarc.example", 443); !d.Allowed {
		t.Errorf("underscore host => %+v", d)
	}
}

// TestNeverRoute_FailsClosedOnNil: the dial pin's backstop must refuse an
// unparseable address, never allow it.
func TestNeverRoute_FailsClosedOnNil(t *testing.T) {
	p := newTestPolicy(t, EgressOpen, nil, nil)
	if why := p.neverRoute(nil, 443, false); why == "" {
		t.Error("nil IP allowed")
	}
	if why := p.neverRoute(net.ParseIP("::7f00:1"), 443, false); why != "loopback" {
		t.Errorf("IPv4-compatible ::127.0.0.1 => %q, want loopback", why)
	}
}

// TestEgressDecide_OwnAddressesAndListeners: the host's own interface
// addresses and rein's own loopback listeners are never-route.
func TestEgressDecide_OwnAddressesAndListeners(t *testing.T) {
	p := newTestPolicy(t, EgressOpen, nil, nil)
	if len(p.localAddrs) == 0 {
		t.Skip("no interface addresses")
	}
	for _, la := range p.localAddrs {
		if la.IsLoopback() {
			continue
		}
		d := p.Decide(context.Background(), la.String(), 443)
		if d.Allowed {
			t.Errorf("own address %s allowed", la)
		}
	}
	// A loopback dial to one of rein's own ports is refused even with the
	// test loopback permit on (the port rule passes because 443 is default).
	q, err := NewEgressPolicy(EgressOpen, nil, nil, []int{443})
	if err != nil {
		t.Fatal(err)
	}
	q.permitLoopback = true
	if d := q.Decide(context.Background(), "127.0.0.1", 443); d.Allowed || !strings.HasSuffix(d.Reason, "own-listener") {
		t.Errorf("own listener port: %+v", d)
	}
}

// TestEgressDecide_ModesAndPorts: restricted allows the list (exact, wildcard,
// host:port) and refuses the rest naming the remedy; open allows any public
// host; non-443 ports need an explicit host:port in either mode.
func TestEgressDecide_ModesAndPorts(t *testing.T) {
	r := newTestPolicy(t, EgressRestricted, []string{"pypi.org", "*.pythonhosted.org", "example.com:8443"}, nil)
	for host, port := range map[string]int{"pypi.org": 443, "files.pythonhosted.org": 443, "example.com": 8443} {
		if d := r.Decide(context.Background(), host, port); !d.Allowed || d.Reason != "allowed-egress-list" {
			t.Errorf("restricted %s:%d => %+v", host, port, d)
		}
	}
	d := r.Decide(context.Background(), "example.com", 443)
	if d.Allowed || d.Reason != "refused-egress-host" || !strings.Contains(d.Message, "rein session allow-domain example.com") || !strings.Contains(d.Message, "on the host") {
		t.Errorf("restricted unlisted host => %+v", d)
	}
	if d := r.Decide(context.Background(), "pypi.org", 8443); d.Allowed || d.Reason != "refused-egress-port" {
		t.Errorf("restricted listed host, unlisted port => %+v", d)
	}
	// "*.pythonhosted.org" must not match the bare suffix host itself.
	if d := r.Decide(context.Background(), "pythonhosted.org", 443); d.Allowed {
		t.Errorf("wildcard matched its bare suffix: %+v", d)
	}

	o := newTestPolicy(t, EgressOpen, nil, nil)
	if d := o.Decide(context.Background(), "example.com", 443); !d.Allowed || d.Reason != "allowed-egress-open" {
		t.Errorf("open example.com => %+v", d)
	}
	if d := o.Decide(context.Background(), "v6ok.example", 443); !d.Allowed {
		t.Errorf("open public v6 => %+v", d)
	}
	if d := o.Decide(context.Background(), "example.com", 8443); d.Allowed || d.Reason != "refused-egress-port" {
		t.Errorf("open mode must not widen ports: %+v", d)
	}
	if d := o.Decide(context.Background(), "example.com", 80); d.Allowed {
		t.Errorf("open mode port 80: %+v", d)
	}
	if d := o.Decide(context.Background(), "nx.example", 443); d.Allowed || d.Reason != "refused-egress-resolve" {
		t.Errorf("unresolvable => %+v", d)
	}
	if d := o.Decide(context.Background(), "bad host", 443); d.Allowed || d.Reason != "refused-egress-syntax" {
		t.Errorf("syntax => %+v", d)
	}
}

// TestEgressDecide_InternalHosts: allow_internal_hosts exempts private ranges
// for that exact host:port only, never loopback/link-local/metadata.
func TestEgressDecide_InternalHosts(t *testing.T) {
	p := newTestPolicy(t, EgressRestricted, nil, []string{"internal.corp:443", "tailnet.corp:8080"})
	if d := p.Decide(context.Background(), "internal.corp", 443); !d.Allowed || d.Reason != "allowed-egress-internal" || !d.AllowPrivate {
		t.Errorf("internal => %+v", d)
	}
	if d := p.Decide(context.Background(), "tailnet.corp", 8080); !d.Allowed {
		t.Errorf("internal cgnat host:port => %+v", d)
	}
	if d := p.Decide(context.Background(), "internal.corp", 8443); d.Allowed {
		t.Errorf("internal host on an unlisted port allowed: %+v", d)
	}
	for _, host := range []string{"loop.corp:443", "metadata.corp:443"} {
		q := newTestPolicy(t, EgressRestricted, nil, []string{host})
		h := strings.TrimSuffix(host, ":443")
		if d := q.Decide(context.Background(), h, 443); d.Allowed {
			t.Errorf("internal opt-in must never reach %s: %+v", h, d)
		}
	}
	for _, bad := range []string{"github.com:443", "probe.rein.internal:443", "internal.corp", "x:0"} {
		if _, err := NewEgressPolicy(EgressRestricted, nil, []string{bad}, nil); err == nil {
			t.Errorf("allow_internal_hosts %q accepted", bad)
		}
	}
	if _, err := NewEgressPolicy(EgressRestricted, []string{"api.github.com:8443"}, nil, nil); err == nil {
		t.Error("allow_domains naming a rein host with a port accepted")
	}
}

// TestDialPin: the dialer's Control hook refuses a never-route target even
// when the decision's address list claims it (the resolve/dial TOCTOU
// backstop).
func TestDialPin(t *testing.T) {
	p := newTestPolicy(t, EgressOpen, nil, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	dec := EgressDecision{Allowed: true, Addrs: []net.IP{net.ParseIP("127.0.0.1")}}
	if c, err := p.dialChecked(context.Background(), dec, port); err == nil {
		c.Close()
		t.Fatal("dial to loopback succeeded despite the pin")
	} else if !strings.Contains(err.Error(), "dial pin refused") {
		t.Errorf("unexpected error: %v", err)
	}
	p.permitLoopback = true
	c, err := p.dialChecked(context.Background(), dec, port)
	if err != nil {
		t.Fatalf("test-permitted loopback dial failed: %v", err)
	}
	c.Close()
}

func TestEmbeddedIPv4(t *testing.T) {
	for in, want := range map[string]string{
		"64:ff9b::7f00:1":            "127.0.0.1",
		"64:ff9b:1::a00:1":           "10.0.0.1",
		"2002:c0a8:101::1":           "192.168.1.1",
		"2001:0:1:2:3:4:80ff:fffe":   "127.0.0.1",
		"2606:2800:220:1:248:1893::": "",
		"10.0.0.1":                   "",
	} {
		got := embeddedIPv4(net.ParseIP(in))
		if (want == "" && got != nil) || (want != "" && (got == nil || got.String() != want)) {
			t.Errorf("embeddedIPv4(%s) = %v, want %q", in, got, want)
		}
	}
}

// TestPeerUIDFromProc drives the /proc parser with synthetic tables: a
// dual-stack peer (tcp6, v4-mapped), TIME_WAIT rows never matching, a foreign
// uid refused, and ambiguity refused.
func TestPeerUIDFromProc(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	hdr := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	// peer 127.0.0.1:40000 (hex 0100007F:9C40) -> listener 127.0.0.1:3128 (0C38)
	peer := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40000}
	local := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 3128}

	tcp := write("tcp", hdr+
		"   0: 0100007F:9C40 0100007F:0C38 01 00000000:00000000 00:00000000 00000000  1000        0 12345 1 0000000000000000 20 4 30 10 -1\n"+
		"   1: 0100007F:9C40 0100007F:0C38 06 00000000:00000000 00:00000000 00000000     0        0 0 3 0000000000000000\n")
	empty6 := write("tcp6", hdr)
	if err := peerUIDFromProc(peer, local, 1000, tcp, empty6); err != nil {
		t.Errorf("same uid: %v", err)
	}
	if err := peerUIDFromProc(peer, local, 1001, tcp, empty6); err == nil {
		t.Error("foreign uid accepted")
	}
	// v4-mapped row in tcp6 only.
	tcp6 := write("tcp6b", hdr+
		"   0: 0000000000000000FFFF00000100007F:9C40 0000000000000000FFFF00000100007F:0C38 01 00000000:00000000 00:00000000 00000000  1000        0 1 1 0000000000000000 20 4 30 10 -1\n")
	empty4 := write("tcp4b", hdr)
	if err := peerUIDFromProc(peer, local, 1000, empty4, tcp6); err != nil {
		t.Errorf("v4-mapped tcp6 row: %v", err)
	}
	// Ambiguous: the same tuple ESTABLISHED in both tables.
	if err := peerUIDFromProc(peer, local, 1000, tcp, tcp6); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguity not refused: %v", err)
	}
	// No row at all.
	if err := peerUIDFromProc(peer, local, 1000, empty4, empty6); err == nil {
		t.Error("missing row accepted")
	}
}
