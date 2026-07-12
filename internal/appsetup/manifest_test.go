package appsetup

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestRandomSuffix_LengthAndCharset(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{10}$`)
	for i := 0; i < 25; i++ {
		s, err := randomSuffix()
		if err != nil {
			t.Fatalf("randomSuffix: %v", err)
		}
		if !re.MatchString(s) {
			t.Errorf("suffix %q does not match [0-9a-f]{10}", s)
		}
	}
}

func TestRandomSuffix_VariesAcrossCalls(t *testing.T) {
	a, _ := randomSuffix()
	b, _ := randomSuffix()
	if a == b {
		t.Errorf("two consecutive suffixes equal: %q == %q (vanishingly unlikely; check entropy)", a, b)
	}
}

func TestBuildManifest_Primary(t *testing.T) {
	m, err := BuildManifest(RolePrimary, 54321, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(m.Name, "rein-primary-") {
		t.Errorf("name = %q, want rein-primary- prefix", m.Name)
	}
	if m.RedirectURL != "http://127.0.0.1:54321/callback" {
		t.Errorf("redirect_url = %q", m.RedirectURL)
	}
	if m.Public {
		t.Errorf("public must be false")
	}
	if m.DefaultEvents == nil || len(m.DefaultEvents) != 0 {
		t.Errorf("default_events should be empty []string{}, got %v", m.DefaultEvents)
	}
	want := map[string]string{
		"contents":      "write",
		"issues":        "write",
		"pull_requests": "write",
		"metadata":      "read",
	}
	for k, v := range want {
		if got := m.DefaultPermissions[k]; got != v {
			t.Errorf("perm %s = %q, want %q", k, got, v)
		}
	}
	if len(m.DefaultPermissions) != len(want) {
		t.Errorf("perm count = %d, want %d", len(m.DefaultPermissions), len(want))
	}
}

func TestBuildManifest_Audit(t *testing.T) {
	m, err := BuildManifest(RoleAudit, 1234, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(m.Name, "rein-audit-") {
		t.Errorf("name = %q", m.Name)
	}
	want := map[string]string{
		"issues":   "write",
		"metadata": "read",
	}
	if len(m.DefaultPermissions) != len(want) {
		t.Errorf("perms = %v, want %v", m.DefaultPermissions, want)
	}
	for k, v := range want {
		if got := m.DefaultPermissions[k]; got != v {
			t.Errorf("perm %s = %q, want %q", k, got, v)
		}
	}
}

func TestBuildManifest_UnknownRole(t *testing.T) {
	if _, err := BuildManifest("unknown", 1, ""); err == nil {
		t.Error("expected error for unknown role")
	}
}

func TestBuildManifest_LabelWovenIntoName(t *testing.T) {
	// A machine label lands between the role and the random guard:
	// rein-<role>-<label>-<shortrand>. The guard is still present (global
	// uniqueness), and the label is sanitized (onboarding-ux-design.md §4).
	m, err := BuildManifest(RolePrimary, 1, "Tom's Laptop!")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(m.Name, "rein-primary-toms-laptop-") {
		t.Errorf("name = %q, want rein-primary-toms-laptop-<rand> prefix", m.Name)
	}
	// The trailing guard is still 10 hex chars (40-bit uniqueness).
	guard := strings.TrimPrefix(m.Name, "rein-primary-toms-laptop-")
	if len(guard) != 10 {
		t.Errorf("guard = %q (len %d), want 10 hex chars", guard, len(guard))
	}
}

func TestBuildManifest_EmptyLabelKeepsLabellessShape(t *testing.T) {
	// A label that sanitizes to nothing (all punctuation / whitespace)
	// must fall back to the pre-label rein-<role>-<shortrand> shape, never
	// emit a double hyphen or a trailing hyphen.
	for _, in := range []string{"", "   ", "!!!", "----"} {
		m, err := BuildManifest(RolePrimary, 1, in)
		if err != nil {
			t.Fatalf("build(%q): %v", in, err)
		}
		if strings.Contains(m.Name, "--") {
			t.Errorf("label %q => name %q has a double hyphen", in, m.Name)
		}
		if strings.TrimPrefix(m.Name, "rein-primary-") == "" || strings.HasSuffix(m.Name, "-") {
			t.Errorf("label %q => malformed name %q", in, m.Name)
		}
	}
}

func TestSanitizeMachineLabel(t *testing.T) {
	cases := map[string]string{
		"Tom's Laptop":     "toms-laptop",
		"ubuntu":           "ubuntu",
		"  MyBox  ":        "mybox",
		"a__b--c":          "a-b-c",
		"!!!":              "",
		"":                 "",
		"UPPER":            "upper",
		"work.vm.internal": "work-vm-internal",
	}
	for in, want := range cases {
		if got := SanitizeMachineLabel(in); got != want {
			t.Errorf("SanitizeMachineLabel(%q) = %q, want %q", in, got, want)
		}
	}
	// Over-length labels are capped and never end in a hyphen.
	long := SanitizeMachineLabel("this-is-a-really-long-hostname-that-exceeds-the-cap")
	if len(long) > maxLabelLen {
		t.Errorf("label not capped: %q (len %d > %d)", long, len(long), maxLabelLen)
	}
	if strings.HasSuffix(long, "-") {
		t.Errorf("capped label ends in hyphen: %q", long)
	}
}

func TestBuildManifest_FreshSuffixPerCall(t *testing.T) {
	// Primary and audit MUST get different suffixes (research anti-
	// pattern §6.4: "Same App name").
	p, err := BuildManifest(RolePrimary, 1, "")
	if err != nil {
		t.Fatalf("primary: %v", err)
	}
	a, err := BuildManifest(RoleAudit, 1, "")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	psuf := strings.TrimPrefix(p.Name, "rein-primary-")
	asuf := strings.TrimPrefix(a.Name, "rein-audit-")
	if psuf == asuf {
		t.Errorf("primary and audit share suffix %q", psuf)
	}
}

func TestRenderAutoPostHTML_RoundTripsManifest(t *testing.T) {
	// The manifest JSON ends up in an HTML attribute. html/template
	// escapes quotes (&#34;); the browser decodes them back at POST
	// time. We mimic that by un-decoding the attribute value and
	// unmarshalling. The point of this test is to lock in that we
	// can recover the original manifest from what the browser will
	// submit.
	m, err := BuildManifest(RolePrimary, 1234, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, err := renderAutoPostHTML(m, "state-nonce-xyz", RolePrimary, 1)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := string(got)
	if !strings.Contains(body, `action="https://github.com/settings/apps/new?state=state-nonce-xyz"`) {
		t.Errorf("missing form action with state nonce; body=\n%s", body)
	}
	if !strings.Contains(body, `name="manifest"`) {
		t.Errorf("missing manifest hidden input")
	}
	if !strings.Contains(body, "document.getElementById('f').submit()") {
		t.Errorf("missing auto-submit script")
	}

	// Extract the manifest attribute value and round-trip parse it.
	// The attribute was rendered via html/template, which replaces
	// `"` with `&#34;` inside attribute values. html.UnescapeString
	// reverses that.
	const startTag = `name="manifest" value="`
	startIdx := strings.Index(body, startTag)
	if startIdx < 0 {
		t.Fatalf("manifest input tag not found")
	}
	rest := body[startIdx+len(startTag):]
	endIdx := strings.Index(rest, `"`)
	if endIdx < 0 {
		t.Fatalf("manifest input close quote not found")
	}
	escaped := rest[:endIdx]
	raw := htmlUnescape(escaped)
	var rt Manifest
	if err := json.Unmarshal([]byte(raw), &rt); err != nil {
		t.Fatalf("unmarshal manifest from attribute: %v\nraw=%q", err, raw)
	}
	if rt.Name != m.Name || rt.RedirectURL != m.RedirectURL {
		t.Errorf("round-trip mismatch: got %+v, want %+v", rt, m)
	}
}

// htmlUnescape is a tiny attribute-value un-escaper sufficient for
// the limited set of escapes html/template emits in attribute context
// for our JSON payload (`"` -> `&#34;`, `&` -> `&amp;`).
func htmlUnescape(s string) string {
	s = strings.ReplaceAll(s, "&#34;", `"`)
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return s
}

func TestManifestPermissions_Role(t *testing.T) {
	p := manifestPermissions(RolePrimary)
	if p["contents"] != "write" {
		t.Errorf("primary contents = %q, want write", p["contents"])
	}
	a := manifestPermissions(RoleAudit)
	if _, ok := a["contents"]; ok {
		t.Errorf("audit must not have contents permission, got %v", a)
	}
	if a["issues"] != "write" {
		t.Errorf("audit issues = %q, want write", a["issues"])
	}
}
