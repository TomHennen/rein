package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/session"
)

func TestNewRunSecret(t *testing.T) {
	a, err := newRunSecret()
	if err != nil || len(a) != 32 {
		t.Fatalf("secret %q err=%v", a, err)
	}
	b, _ := newRunSecret()
	if a == b {
		t.Error("two secrets were equal")
	}
}

func TestBuildEgressPolicy_ModeFollowsSession(t *testing.T) {
	r, err := buildEgressPolicy(session.Session{ID: "x", Repos: []string{"o/r"}}, []string{"pypi.org"})
	if err != nil || r.Mode != proxy.EgressRestricted {
		t.Errorf("restricted: mode=%v err=%v", r.Mode, err)
	}
	o, err := buildEgressPolicy(session.Session{ID: "x", Repos: []string{"o/r"}, OpenEgress: true}, nil)
	if err != nil || o.Mode != proxy.EgressOpen {
		t.Errorf("open: mode=%v err=%v", o.Mode, err)
	}
	if _, err := buildEgressPolicy(session.Session{ID: "x", Repos: []string{"o/r"}, AllowInternalHosts: []string{"github.com:443"}}, nil); err == nil {
		t.Error("internal host naming a rein host accepted")
	}
}

// TestBuildAgentContract_OpenEgress: open mode replaces the allowlist facts
// with the open-mode facts and drops the allow-domain self-help (it is not
// the remedy any more); restricted keeps them.
func TestBuildAgentContract_OpenEgress(t *testing.T) {
	open := buildAgentContract(contractParams{WorkTree: "/w", HomeEphemeral: true, OpenEgress: true, ExtraDomains: []string{"pypi.org"}})
	for _, want := range []string{"Egress is OPEN", "untrusted input", "Still refused", "localhost", "allow_internal_hosts"} {
		if !strings.Contains(open, want) {
			t.Errorf("open contract missing %q\n%s", want, open)
		}
	}
	if strings.Contains(open, "restricted to GitHub") || strings.Contains(open, "rein session allow-domain") {
		t.Errorf("open contract still carries restricted-mode text\n%s", open)
	}
	restricted := buildAgentContract(contractParams{WorkTree: "/w", HomeEphemeral: true, ExtraDomains: []string{"pypi.org"}})
	if !strings.Contains(restricted, "restricted to GitHub plus: pypi.org") || strings.Contains(restricted, "Egress is OPEN") {
		t.Errorf("restricted contract wrong\n%s", restricted)
	}
}

func TestOpenEgressBanner(t *testing.T) {
	var b bytes.Buffer
	printOpenEgressBanner(&b, "/home/x/.config/rein/dev-session.yaml")
	for _, want := range []string{"EGRESS IS OPEN", "dev-session.yaml", "ANY public host on port 443", "untrusted input", "audit log", "open-egress off"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("banner missing %q:\n%s", want, b.String())
		}
	}
}
