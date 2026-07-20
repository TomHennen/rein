package proxy

import "testing"

// TestInjectAndCDNHostsMatchClassifier is the drift guard between the exported
// host lists (consumed by internal/nono to build the profile's allow_domain +
// upstream_bypass) and classifyHost (the runtime injection decision). If someone
// adds a host to one but not the other, the sandbox allowlist and the injector would
// disagree — a token could leak or an op could silently bypass injection.
func TestInjectAndCDNHostsMatchClassifier(t *testing.T) {
	for _, h := range InjectHosts {
		switch classifyHost(h) {
		case classInjectBearer, classInjectBasic:
			// ok — an inject host must classify as an inject class.
		default:
			t.Errorf("InjectHosts contains %q but classifyHost does not treat it as an inject class", h)
		}
	}
	for _, h := range CDNHosts {
		if classifyHost(h) != classPassthrough {
			t.Errorf("CDNHosts contains %q but classifyHost does not treat it as passthrough", h)
		}
	}
	// No overlap: an inject host must never also be a CDN host.
	cdn := map[string]bool{}
	for _, h := range CDNHosts {
		cdn[h] = true
	}
	for _, h := range InjectHosts {
		if cdn[h] {
			t.Errorf("%q is in both InjectHosts and CDNHosts", h)
		}
	}
}
