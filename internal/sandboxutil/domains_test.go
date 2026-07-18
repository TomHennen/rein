package sandboxutil

import (
	"strings"
	"testing"
)

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestResolveExtraDomainsDefaultAlwaysPresent: with no custom sources, the
// built-in default (the wrapped agent's API endpoint) is present and is the
// only entry — no telemetry/MCP hosts sneak in.
func TestResolveExtraDomainsDefaultAlwaysPresent(t *testing.T) {
	got, warns, err := ResolveExtraAllowedDomains(nil, "")
	if err != nil {
		t.Fatalf("ResolveExtraAllowedDomains: %v", err)
	}
	if !contains(got, "api.anthropic.com") {
		t.Errorf("default api.anthropic.com missing: %v", got)
	}
	if len(got) != 1 {
		t.Errorf("default set should be exactly the agent endpoint, got %v", got)
	}
	if len(warns) != 0 {
		t.Errorf("no warnings expected for the bare default, got %v", warns)
	}
}

// TestResolveExtraDomainsUnionAndDedupe: env + session are unioned with the
// default and deduped case-insensitively; the default cannot be dropped.
func TestResolveExtraDomainsUnionAndDedupe(t *testing.T) {
	got, _, err := ResolveExtraAllowedDomains(
		[]string{"pypi.org", "Registry.NPMJS.org", "api.anthropic.com."},
		"registry.npmjs.org, files.pythonhosted.org",
	)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, want := range []string{"api.anthropic.com", "pypi.org", "registry.npmjs.org", "files.pythonhosted.org"} {
		if !contains(got, want) {
			t.Errorf("merged set missing %q: %v", want, got)
		}
	}
	// registry.npmjs.org appeared in BOTH env and session with different case +
	// a trailing dot on the default duplicate — all must collapse.
	seen := map[string]int{}
	for _, d := range got {
		seen[d]++
		if seen[d] > 1 {
			t.Errorf("duplicate domain %q in merged set %v", d, got)
		}
	}
	if len(got) != 4 {
		t.Errorf("expected 4 unique domains, got %d: %v", len(got), got)
	}
}

// TestResolveExtraDomainsWildcardWarns: a *.suffix wildcard is ALLOWED (egress
// is the operator's choice) but produces a loud exfil warning.
func TestResolveExtraDomainsWildcardWarns(t *testing.T) {
	got, warns, err := ResolveExtraAllowedDomains([]string{"*.internal.example.com"}, "")
	if err != nil {
		t.Fatalf("resolve rejected a legal *.suffix wildcard: %v", err)
	}
	if !contains(got, "*.internal.example.com") {
		t.Errorf("wildcard domain missing from allowlist: %v", got)
	}
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, " "), "*.internal.example.com") {
		t.Errorf("expected an exfil warning naming the wildcard, got %v", warns)
	}
}

// TestResolveExtraDomainsLargeSetWarns: more than the threshold of custom hosts
// triggers a "keep this minimal" warning.
func TestResolveExtraDomainsLargeSetWarns(t *testing.T) {
	many := []string{"a.com", "b.com", "c.com", "d.com", "e.com", "f.com", "g.com", "h.com", "i.com"}
	_, warns, err := ResolveExtraAllowedDomains(many, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, " "), "custom egress domains") {
		t.Errorf("expected a large-set warning, got %v", warns)
	}
}

// TestResolveExtraDomainsRejectsMalformed: schemes, paths, ports, bare * and
// malformed wildcards fail closed rather than silently allow a bogus entry.
func TestResolveExtraDomainsRejectsMalformed(t *testing.T) {
	bad := []string{
		"https://api.anthropic.com",
		"example.com/path",
		"example.com:443",
		"*",
		"foo.*",
		"a.*.b",
		"has space.com",
		"a\nb.com",   // embedded newline (not trimmed by TrimSpace)
		"a\tb.com",   // embedded tab
		"a\x00b.com", // embedded NUL
	}
	for _, b := range bad {
		if _, _, err := ResolveExtraAllowedDomains([]string{b}, ""); err == nil {
			t.Errorf("resolve accepted malformed domain %q", b)
		}
	}
}

// TestResolveExtraDomainsGitHubHostDedupesNotErrors: listing a GitHub inject
// host in allow_domains must NOT error (it can't create an injection gap — the
// injector is driven by mitmProxy.domains) — it just dedupes.
func TestResolveExtraDomainsGitHubHostDedupesNotErrors(t *testing.T) {
	got, _, err := ResolveExtraAllowedDomains([]string{"github.com", "api.github.com"}, "")
	if err != nil {
		t.Fatalf("resolve should tolerate a GitHub host in allow_domains: %v", err)
	}
	if !contains(got, "github.com") {
		t.Errorf("github.com should be present: %v", got)
	}
}
