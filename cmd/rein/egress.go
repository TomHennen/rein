// Launch-side glue for the #185 external-proxy shape: the per-run secrets and
// the egress policy rein enforces in place of srt.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

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
