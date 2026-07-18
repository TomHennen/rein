package sandboxutil

import (
	"fmt"
	"sort"
	"strings"
)

// DefaultExtraAllowedDomains: built-in egress allowlist so the wrapped agent
// reaches its own API out of the box. Egress-allowed but NEVER injected (no rein
// token). Just api.anthropic.com — a headless `claude -p` contacts only that host
// (CP4.5); telemetry/MCP are excluded to keep the exfil surface minimal. More
// hosts are opt-in via allow_domains or REIN_ALLOW_DOMAINS.
var DefaultExtraAllowedDomains = []string{"api.anthropic.com"}

// EnvAllowDomains is the machine-wide extra-egress override: a comma-separated
// list of hosts merged into the allowlist (union) on every sandboxed run.
const EnvAllowDomains = "REIN_ALLOW_DOMAINS"

// largeExtraSetThreshold is the count of CUSTOM (non-default) extra domains above
// which ResolveExtraAllowedDomains warns about the egress-exfiltration surface.
// Kept small so a broad allowlist is always called out loudly.
const largeExtraSetThreshold = 8

// ResolveExtraAllowedDomains unions the extra egress allowlist from all sources
// (DefaultExtraAllowedDomains + REIN_ALLOW_DOMAINS + per-session allow_domains)
// into one lowercased, deduped list. It returns warnings about the exfil surface
// (each wildcard, and a large custom set) for the caller to print, and fails
// closed on a malformed domain (scheme/path/space/port, or a non-`*.suffix` wildcard).
func ResolveExtraAllowedDomains(sessionDomains []string, envValue string) (domains, warnings []string, err error) {
	custom := append(splitAndTrim(envValue), sessionDomains...)

	seen := map[string]bool{}
	out := make([]string, 0, len(DefaultExtraAllowedDomains)+len(custom))
	add := func(raw string) (added bool, e error) {
		d := normalizeDomain(raw)
		if d == "" {
			return false, nil
		}
		if e := validateEgressDomain(d); e != nil {
			return false, e
		}
		if seen[d] {
			return false, nil
		}
		seen[d] = true
		out = append(out, d)
		return true, nil
	}

	// Defaults first (always present), then the custom sources (union).
	for _, d := range DefaultExtraAllowedDomains {
		if _, e := add(d); e != nil {
			return nil, nil, fmt.Errorf("invalid default egress domain %q: %w", d, e)
		}
	}
	var customCount int
	var wildcards []string
	for _, raw := range custom {
		added, e := add(raw)
		if e != nil {
			return nil, nil, fmt.Errorf("invalid extra allowed domain %q: %w", raw, e)
		}
		if added {
			customCount++
			if strings.Contains(normalizeDomain(raw), "*") {
				wildcards = append(wildcards, normalizeDomain(raw))
			}
		}
	}
	sort.Strings(out)

	for _, w := range wildcards {
		warnings = append(warnings, fmt.Sprintf("wildcard egress domain %q lets the sandboxed agent reach ANY subdomain of it — a data-exfiltration surface", w))
	}
	if customCount > largeExtraSetThreshold {
		warnings = append(warnings, fmt.Sprintf("%d custom egress domains allowed — the sandboxed agent may send data to any of them; keep this list minimal", customCount))
	}
	return out, warnings, nil
}

// normalizeDomain lowercases, trims whitespace, and drops a trailing FQDN dot.
func normalizeDomain(d string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(d), ".")))
}

// validateEgressDomain rejects entries that are not a bare host or a strict
// `*.suffix` wildcard — the only two forms srt's domain matcher accepts. d is
// expected already normalized (lowercased, trimmed).
func validateEgressDomain(d string) error {
	if d == "" {
		return fmt.Errorf("empty")
	}
	if strings.Contains(d, "://") {
		return fmt.Errorf("must be a bare host, not a URL (no scheme)")
	}
	if strings.Contains(d, "/") {
		return fmt.Errorf("must be a bare host (no path)")
	}
	// Reject ANY whitespace or control char (space, tab, but also embedded
	// \n\r\v\f that TrimSpace on the ends does not remove) — fail closed on a
	// malformed entry rather than emit a dead allowlist string.
	for _, r := range d {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("must not contain whitespace or control characters")
		}
	}
	if strings.Contains(d, ":") {
		return fmt.Errorf("must not include a port")
	}
	if strings.Contains(d, "*") {
		if d == "*" || !strings.HasPrefix(d, "*.") || strings.Count(d, "*") != 1 {
			return fmt.Errorf("a wildcard must be of the form *.suffix (a bare * would allow ALL egress)")
		}
	}
	return nil
}

// splitAndTrim splits a comma-separated list and trims each element, dropping
// empties. Whitespace and empty fields (e.g. a trailing comma) are tolerated.
func splitAndTrim(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
