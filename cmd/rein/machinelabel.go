package main

// Machine label (onboarding-ux-design.md §4, §8.1).
//
// The App name is `rein-<role>-<label>-<shortrand>`. The label makes a
// per-machine App human-recognizable (so a `toms-laptop[bot]` commit is
// distinguishable from `toms-workvm[bot]`), while the random guard keeps
// the globally-unique GitHub App name unique.
//
// The label defaults to the sanitized hostname WHEN DISTINCTIVE. On a
// generic hostname (`ubuntu`, `localhost`, …) the default won't tell two
// machines apart, so on a real terminal init PROMPTS — pre-filled with the
// detected hostname, editable (decision §8.1: "prompt"). Non-interactive
// runs (no tty / --yes / headless) never block: they fall back to the
// hostname-or-default label (guardrail §7).

import (
	"fmt"
	"os"
	"strings"

	"github.com/TomHennen/rein/internal/appsetup"
)

// defaultMachineLabel is the label used when the hostname yields nothing
// usable (empty/garbage) and the run is non-interactive. It is intentionally
// generic — a real user on a tty is prompted to replace it.
const defaultMachineLabel = "dev"

// genericHostnames are hostnames that do NOT distinguish one machine from
// another, so the hostname default is a poor App label. On these, the
// prompt matters most (design §3 step 3: "prompt more insistently").
var genericHostnames = map[string]bool{
	"":            true,
	"localhost":   true,
	"ubuntu":      true,
	"debian":      true,
	"linux":       true,
	"vagrant":     true,
	"raspberrypi": true,
	"kali":        true,
	"fedora":      true,
	"archlinux":   true,
	"vm":          true,
	"box":         true,
}

// hostnameForLabel returns the machine hostname used to seed the label.
//
// REIN_MACHINE_HOSTNAME is an explicit override, present ONLY so tests and
// demo journeys can pin the pre-filled default to a deterministic value
// (os.Hostname() returns whatever the running box is called, which would
// bake the operator's hostname into a golden). It is documented as a test
// seam; production leaves it unset and os.Hostname() is used.
func hostnameForLabel() string {
	if h := strings.TrimSpace(os.Getenv("REIN_MACHINE_HOSTNAME")); h != "" {
		return h
	}
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// isDistinctiveHostname reports whether the RAW hostname (before
// sanitization) is specific enough to distinguish this machine. Generic
// distro/default names are not.
func isDistinctiveHostname(raw string) bool {
	return !genericHostnames[strings.ToLower(strings.TrimSpace(raw))]
}

// resolveMachineLabel picks the machine label for App naming, applying the
// precedence: --machine-label flag > interactive prompt > hostname-or-default.
//
// The prompt fires ONLY on a real terminal with assumeYes false (guardrail
// §7: non-interactive fallback is mandatory). When it does fire it is
// pre-filled with the detected hostname label and editable; a bare Enter
// accepts the pre-fill. Headless/CI/piped/--yes never prompt and fall back
// to the hostname label (or defaultMachineLabel when the hostname is
// unusable) — init must never hang.
//
// The returned label is always sanitized (SanitizeMachineLabel), so the
// value the user sees is exactly what reaches BuildManifest.
func resolveMachineLabel(flag string, stdin *os.File, assumeYes bool) string {
	if f := appsetup.SanitizeMachineLabel(flag); f != "" {
		return f
	}

	rawHost := hostnameForLabel()
	hostLabel := appsetup.SanitizeMachineLabel(rawHost)

	// Non-interactive: fall back without blocking.
	if assumeYes || !stdinIsTerminal(stdin) {
		if hostLabel != "" {
			return hostLabel
		}
		return defaultMachineLabel
	}

	// Interactive: prompt, pre-filled with the hostname label. On a generic
	// hostname the pre-fill is a weak default, so lead with a nudge; either
	// way Enter accepts the pre-fill.
	def := hostLabel
	if def == "" {
		def = defaultMachineLabel
	}
	if !isDistinctiveHostname(rawHost) {
		fmt.Fprintf(os.Stdout,
			"  (hostname %q is generic — a distinctive name makes per-machine [bot] commits recognizable)\n",
			rawHost)
	}
	answer := promptWithDefault(os.Stdout, stdin, "Name this machine", def)
	label := appsetup.SanitizeMachineLabel(answer)
	if label == "" {
		return def
	}
	return label
}
