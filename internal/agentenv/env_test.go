package agentenv

import "testing"

// TestDisableClaudeMCPFromEnv covers the truthy parser: only explicit truthy
// values opt out; everything else (unset/empty/"0"/garbage) keeps MCP enabled.
func TestDisableClaudeMCPFromEnv(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " On "} {
		if !DisableClaudeMCPFromEnv(v) {
			t.Errorf("DisableClaudeMCPFromEnv(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "nope", "2"} {
		if DisableClaudeMCPFromEnv(v) {
			t.Errorf("DisableClaudeMCPFromEnv(%q) = true, want false", v)
		}
	}
}
