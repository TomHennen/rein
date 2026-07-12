package srt

import (
	"fmt"
	"path/filepath"
	"strings"
)

// EnvSandboxAllowRead is the operator escape hatch for the wholesale $HOME
// deny-read (issue #59): a colon-separated list of ABSOLUTE paths to allow
// back READ-ONLY inside the sandbox, merged with rein's auto-derived
// allow-back set. This is the intended remediation when a tool breaks on a
// hidden $HOME path — the run banner prints the exact syntax. Entries are
// symlink-resolved and overlap-checked by Build: one can never re-expose a
// credential-store deny (fail closed), and allowing $HOME itself is rejected
// in favor of the explicit kill switch below.
const EnvSandboxAllowRead = "REIN_SANDBOX_ALLOW_READ"

// EnvSandboxShowHome is the LOUD kill switch: a truthy value ("1", "true",
// "yes", "on") skips the wholesale $HOME deny entirely, leaving only the
// targeted credential denials. Every unknown-unknown credential store in the
// home directory becomes readable in-sandbox again, so the caller must print
// an unmissable banner warning whenever this is set. There is deliberately NO
// interactive "allow this dir?" prompt (issue #59: rubber-stamping risk); the
// choices are the narrow env allowlist above or this explicit, global opt-out.
const EnvSandboxShowHome = "REIN_SANDBOX_SHOW_HOME"

// ParseSandboxAllowRead parses the REIN_SANDBOX_ALLOW_READ value: a
// colon-separated list of absolute paths (the PATH-style list convention).
// Empty segments (stray/trailing colons) are tolerated and dropped; a
// relative or "~" entry is a hard error — fail closed rather than guess what
// the operator meant to expose. Symlink resolution and the
// credential-overlap rejection happen later, in Build, alongside the
// auto-derived entries.
func ParseSandboxAllowRead(v string) ([]string, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(v, ":") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("%s entry %q must be an absolute path (no ~ or relative paths; fail closed rather than guess)", EnvSandboxAllowRead, p)
		}
		out = append(out, filepath.Clean(p))
	}
	return out, nil
}

// ShowHomeFromEnv reports whether the given REIN_SANDBOX_SHOW_HOME value opts
// OUT of hiding $HOME. Only an explicit truthy value disables the deny;
// anything else (unset, empty, "0", "false", garbage) keeps the default-on
// protection. Same truthy set as DisableClaudeMCPFromEnv for consistency.
func ShowHomeFromEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
