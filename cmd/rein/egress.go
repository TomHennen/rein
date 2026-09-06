// Launch-side glue for the #185 external-proxy shape: the per-run secrets and
// the egress policy rein enforces in place of srt.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/session"
)

// newRunSecret returns 32 hex chars from crypto/rand (the proxy secret, the
// probe nonce). Fails closed: a weak or empty secret is never substituted.
func newRunSecret() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate run secret: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// printOpenEgressBanner is the fixed, non-suppressible disclosure for open
// mode (#185): what it lifts and what it does not.
func printOpenEgressBanner(w io.Writer, sessionFile string) {
	fmt.Fprintln(w, "===============================================================")
	fmt.Fprintln(w, "rein: EGRESS IS OPEN (open_egress: true in "+sessionFile+")")
	fmt.Fprintln(w, "  The sandboxed agent can reach ANY public host on port 443. It can send")
	fmt.Fprintln(w, "  anything it can read — your checkout, its own transcript, its Anthropic")
	fmt.Fprintln(w, "  credential — to any site on the internet, and everything it reads from")
	fmt.Fprintln(w, "  the web is untrusted input. GitHub credential hiding and write brokering")
	fmt.Fprintln(w, "  are unchanged; loopback, private and link-local targets and other ports")
	fmt.Fprintln(w, "  stay refused; every host it contacts is in this run's audit log.")
	fmt.Fprintln(w, "  Turn it off: rein session open-egress off")
	fmt.Fprintln(w, "===============================================================")
}

// buildEgressPolicy turns the session + the resolved extra-egress union into
// the policy the TCP listener enforces. domains is the same list srt used to
// receive (defaults + preset + env + session), so restricted mode is
// behavior-identical to the mitm shape.
func buildEgressPolicy(sess session.Session, domains []string) (*proxy.EgressPolicy, error) {
	mode := proxy.EgressRestricted
	if sess.OpenEgress {
		mode = proxy.EgressOpen
	}
	return proxy.NewEgressPolicy(mode, domains, sess.AllowInternalHosts, sess.ExposePorts)
}
