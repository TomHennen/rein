// Egress policy for the forward-proxy leg (#185, docs/open-egress-design.md):
// rein, not srt, decides whether a CONNECT to a non-GitHub host proceeds. The
// gate runs in a fixed order — host syntax, port, never-route (unconditional,
// resolved once, pinned at dial), then mode (restricted allowlist or open).
package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// EgressMode selects how non-GitHub CONNECTs are judged.
type EgressMode int

const (
	// EgressRestricted allows only the configured host list (today's behavior).
	EgressRestricted EgressMode = iota
	// EgressOpen allows any host that passes syntax, port, and never-route.
	EgressOpen
)

func (m EgressMode) String() string {
	if m == EgressOpen {
		return "open"
	}
	return "restricted"
}

// Tunnel bounds (design "Tunnel bounds").
const (
	egressDialTimeout    = 15 * time.Second
	egressIdleTimeout    = 10 * time.Minute
	maxConcurrentTunnels = 256
	egressDefaultPort    = 443
)

// EgressPolicy is one run's egress decision table. Build with NewEgressPolicy.
type EgressPolicy struct {
	Mode EgressMode

	exact    map[string]bool // bare hosts allowed on the default port
	suffixes []string        // "*.suffix" entries, stored as ".suffix"
	ports    map[string]bool // "host:port" entries (non-default ports)
	internal map[string]bool // "host:port" internal hosts (private-range exemption)

	// listenerPorts are rein's own loopback ports (the proxy, expose_ports);
	// any address on them is never-route regardless of range.
	listenerPorts map[int]bool
	// localAddrs are the host's interface addresses at construction.
	localAddrs []net.IP

	// Resolve is the name resolver (tests inject). Nil uses net.DefaultResolver.
	Resolve func(ctx context.Context, host string) ([]net.IPAddr, error)

	// permitLoopback and defaultPort are package-internal TEST hooks: unit
	// tests have no non-loopback upstream on port 443 to tunnel to. Unexported,
	// never set from config; defaultPort 0 means egressDefaultPort.
	permitLoopback bool
	defaultPort    int
}

func (p *EgressPolicy) defPort() int {
	if p.defaultPort != 0 {
		return p.defaultPort
	}
	return egressDefaultPort
}

// NewEgressPolicy builds the table. domains are normalized entries as
// srt.ResolveExtraAllowedDomains emits them (bare host, "*.suffix", or
// "host:port"); internal are "host:port" internal-host opt-ins.
func NewEgressPolicy(mode EgressMode, domains, internal []string, listenerPorts []int) (*EgressPolicy, error) {
	p := &EgressPolicy{
		Mode:          mode,
		exact:         map[string]bool{},
		ports:         map[string]bool{},
		internal:      map[string]bool{},
		listenerPorts: map[int]bool{},
	}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		switch {
		case d == "":
			continue
		case strings.HasPrefix(d, "*."):
			if strings.Count(d, "*") != 1 || len(d) < 4 {
				return nil, fmt.Errorf("egress: bad wildcard %q", d)
			}
			p.suffixes = append(p.suffixes, d[1:])
		case strings.Contains(d, ":"):
			host, port, err := splitHostPortStrict(d)
			if err != nil {
				return nil, fmt.Errorf("egress: %q: %w", d, err)
			}
			if reservedHost(host) {
				return nil, fmt.Errorf("egress: %q: rein's own hosts accept only port 443", d)
			}
			p.ports[net.JoinHostPort(host, strconv.Itoa(port))] = true
		default:
			p.exact[d] = true
		}
	}
	for _, d := range internal {
		host, port, err := splitHostPortStrict(strings.ToLower(strings.TrimSpace(d)))
		if err != nil {
			return nil, fmt.Errorf("egress: allow_internal_hosts %q: %w (want host:port)", d, err)
		}
		if reservedHost(host) {
			return nil, fmt.Errorf("egress: allow_internal_hosts %q names a rein host", d)
		}
		p.internal[net.JoinHostPort(host, strconv.Itoa(port))] = true
	}
	for _, lp := range listenerPorts {
		p.listenerPorts[lp] = true
	}
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				p.localAddrs = append(p.localAddrs, ipn.IP)
			}
		}
	}
	return p, nil
}

// reservedHost reports whether host is one rein terminates or answers itself.
func reservedHost(host string) bool {
	if host == ProbeHost {
		return true
	}
	for _, h := range InjectHosts {
		if host == h {
			return true
		}
	}
	for _, h := range CDNHosts {
		if host == h {
			return true
		}
	}
	for _, h := range LocalHosts {
		if host == h {
			return true
		}
	}
	return false
}

func splitHostPortStrict(s string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("bad port %q", portStr)
	}
	if err := validHostSyntax(host); err != nil {
		return "", 0, err
	}
	return host, port, nil
}

// validHostSyntax mirrors srt's isValidHost: bounded length, DNS/IP-literal
// charset, no percent (zone ids), no control characters.
func validHostSyntax(host string) error {
	if host == "" || len(host) > 255 {
		return errors.New("empty or over-long host")
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '.', r == ':', r == '[', r == ']':
		default:
			return fmt.Errorf("host contains %q", r)
		}
	}
	return nil
}

// EgressDecision is the gate's answer for one CONNECT.
type EgressDecision struct {
	Allowed bool
	// Reason is the audit tag: allowed-egress-list / allowed-egress-open /
	// allowed-egress-internal, or refused-egress-<why>.
	Reason string
	// Message is the human/agent-facing refusal text (never a token).
	Message string
	// Addrs are the checked addresses the dial MUST use (never re-resolve).
	Addrs []net.IP
	// AllowPrivate is set for an internal-host match: the dial pin exempts the
	// private ranges (never loopback/link-local/own addresses).
	AllowPrivate bool
}

// Decide runs the gate for a CONNECT host:port.
func (p *EgressPolicy) Decide(ctx context.Context, host string, port int) EgressDecision {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	refuse := func(why, msg string) EgressDecision {
		return EgressDecision{Reason: "refused-egress-" + why, Message: msg}
	}
	if err := validHostSyntax(host); err != nil {
		return refuse("syntax", "rein: malformed CONNECT host")
	}
	hp := net.JoinHostPort(host, strconv.Itoa(port))
	internal := p.internal[hp]
	if port != p.defPort() && !p.ports[hp] && !internal {
		return refuse("port", fmt.Sprintf("rein: port %d is not allowed (only 443, or a host:port the human listed in allow_domains)", port))
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return refuse("loopback", "rein: loopback and the host's own services are never reachable from the sandbox")
	}
	// Resolve once; every address must pass never-route.
	addrs, err := p.resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return refuse("resolve", fmt.Sprintf("rein: could not resolve %s", host))
	}
	for _, a := range addrs {
		if why := p.neverRoute(a, port, internal); why != "" {
			return refuse(why, fmt.Sprintf("rein: %s resolves to a %s address; the sandbox never reaches loopback, private, or link-local targets (allow_internal_hosts is the host-side opt-in for private ones)", host, why))
		}
	}
	switch {
	case internal:
		return EgressDecision{Allowed: true, Reason: "allowed-egress-internal", Addrs: addrs, AllowPrivate: true}
	case p.Mode == EgressOpen:
		return EgressDecision{Allowed: true, Reason: "allowed-egress-open", Addrs: addrs}
	case p.listed(host, hp):
		return EgressDecision{Allowed: true, Reason: "allowed-egress-list", Addrs: addrs}
	}
	return refuse("host", fmt.Sprintf("rein: %s is not allowed for this run. Ask the human to run, on the host: rein session allow-domain %s (takes effect on the next run)", host, host))
}

func (p *EgressPolicy) listed(host, hp string) bool {
	if p.exact[host] || p.ports[hp] {
		return true
	}
	for _, s := range p.suffixes {
		if strings.HasSuffix(host, s) && len(host) > len(s) {
			return true
		}
	}
	return false
}

func (p *EgressPolicy) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return []net.IP{ip}, nil
	}
	r := p.Resolve
	if r == nil {
		r = net.DefaultResolver.LookupIPAddr
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ias, err := r(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(ias))
	for _, ia := range ias {
		out = append(out, ia.IP)
	}
	return out, nil
}

// neverRoute returns the reason an address must not be dialed, or "".
// allowPrivate exempts the private/ULA/CGNAT ranges only (internal hosts).
func (p *EgressPolicy) neverRoute(ip net.IP, port int, allowPrivate bool) string {
	if v4 := embeddedIPv4(ip); v4 != nil {
		ip = v4
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	// rein's own listeners first: they are never-route even under the test
	// loopback permit.
	if p.listenerPorts[port] && (ip.IsLoopback() || ip.IsUnspecified()) {
		return "own-listener"
	}
	switch {
	case ip.IsLoopback() && p.permitLoopback:
		return ""
	case ip.IsUnspecified(), ip.IsLoopback():
		return "loopback"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return "link-local"
	case ip.To4() != nil && (ip.To4()[0] == 0 || ip.To4()[0] >= 240):
		return "reserved"
	case len(ip) == net.IPv6len && ip[0] == 0xfe && ip[1]&0xc0 == 0xc0: // fec0::/10 site-local
		return "link-local"
	}
	for _, la := range p.localAddrs {
		if la.Equal(ip) {
			return "own-address"
		}
	}
	if ip.IsPrivate() || isCGNAT(ip) {
		if allowPrivate {
			return ""
		}
		return "private"
	}
	return ""
}

func isCGNAT(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 64 // 100.64.0.0/10
}

// embeddedIPv4 extracts the IPv4 address carried by NAT64 (64:ff9b::/96,
// 64:ff9b:1::/48), 6to4 (2002::/16), and Teredo (2001::/32, XOR ffffffff) so
// the never-route check sees the real target.
func embeddedIPv4(ip net.IP) net.IP {
	ip = ip.To16()
	if ip == nil || ip.To4() != nil {
		return nil
	}
	switch {
	case ip[0] == 0 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b:
		// 64:ff9b::/96 (last 4 bytes) and 64:ff9b:1::/48 (same tail position)
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	case ip[0] == 0x20 && ip[1] == 0x02:
		return net.IPv4(ip[2], ip[3], ip[4], ip[5])
	case ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0 && ip[3] == 0:
		x := binary.BigEndian.Uint32(ip[12:16]) ^ 0xffffffff
		return net.IPv4(byte(x>>24), byte(x>>16), byte(x>>8), byte(x))
	}
	return nil
}

// dialChecked dials the FIRST reachable checked address, with the never-route
// pin re-applied inside the dialer on the concrete ip:port (the TOCTOU
// backstop). No parent proxy is ever consulted.
func (p *EgressPolicy) dialChecked(ctx context.Context, d EgressDecision, port int) (net.Conn, error) {
	var last error
	for _, ip := range d.Addrs {
		dialer := &net.Dialer{
			Timeout: egressDialTimeout,
			Control: func(network, address string, c syscall.RawConn) error {
				h, ps, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				cp, _ := strconv.Atoi(ps)
				if why := p.neverRoute(net.ParseIP(h), cp, d.AllowPrivate); why != "" {
					return fmt.Errorf("rein: dial pin refused %s (%s)", address, why)
				}
				return nil
			},
		}
		c, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		if err == nil {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
			}
			return c, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("no addresses")
	}
	return nil, last
}
