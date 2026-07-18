// Package containment is the config-derived ORACLE for the sandbox containment
// probe harness (issue #136B, docs/containment-probe-harness.md).
//
// sandbox-probe (github.com/controlplaneio/sandbox-probe, Apache-2.0) supplies
// the ENUMERATION: run on the host unconfined, run again inside the srt sandbox,
// diff. sandbox-probe is invoked as an external process (like pyte in
// tests/interactive) and is NEVER imported here — its license never touches the
// shipped binary (hard-constraint #4). This package supplies the one piece it
// cannot: the judgement of whether a surviving-reachable observation is EXPECTED
// (egress allowlist, a CDN host getting direct un-injected TLS, the writable
// working tree) or a LEAK (a credential store still readable, a denied host
// still reachable, a token injected onto a non-inject host).
//
// The oracle consumes rein's EMITTED sandbox config (srt.Config, unmarshaled
// from the settings.json rein wrote) so the expected/denied sets are the real
// per-run sets, never a hand-maintained copy that can drift.
package containment

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/srt"
)

// Kind is the channel an observation belongs to.
type Kind string

const (
	KindNetwork Kind = "network" // egress reachability + token injection
	KindFile    Kind = "file"    // sensitive-path readability
	KindEnv     Kind = "env"     // sensitive env-var presence
)

// Observation is one normalized finding from the differential probe, describing
// what the IN-SANDBOX run saw. The harness maps sandbox-probe's native report
// into this shape (see README: the mapping is the current stub).
type Observation struct {
	Kind   Kind   `json:"kind"`
	Target string `json:"target"` // host, absolute path, or env-var name

	// Reachable is the in-sandbox result: host connectable / file readable /
	// env var present. This is the security-relevant bit.
	Reachable bool `json:"reachable"`

	// TokenInjected (network only): a rein credential was observed on the
	// request to Target. Must be true IFF Target is an inject host.
	TokenInjected bool `json:"tokenInjected,omitempty"`
}

// Verdict is the oracle's classification of one observation.
type Verdict string

const (
	// VerdictOK: the observation matches intent (expected-open reachable, or
	// expected-denied blocked, or token present exactly on an inject host).
	VerdictOK Verdict = "ok"
	// VerdictLeak: a containment failure — a denied channel survived
	// confinement, or a token appeared where it must never appear. Any leak
	// fails the harness.
	VerdictLeak Verdict = "leak"
	// VerdictRegression: an EXPECTED-open channel is unexpectedly closed. Not a
	// security failure, but it means the sandbox broke a path the agent needs;
	// surfaced for review, does not fail the run by default.
	VerdictRegression Verdict = "regression"
	// VerdictUnknown: outside the oracle's config-derived knowledge (e.g. a
	// path neither denied nor obviously benign). Reported for human triage,
	// never silently passed.
	VerdictUnknown Verdict = "unknown"
)

// Result pairs an observation with the oracle's verdict and a short reason.
type Result struct {
	Observation Observation `json:"observation"`
	Verdict     Verdict     `json:"verdict"`
	Reason      string      `json:"reason"`
}

// SensitiveEnv are env-var names that must NEVER survive into the sandbox
// (docs/containment-probe-harness.md channel table). rein's env allowlist is
// build-time, not carried in settings.json, so this denylist is encoded here
// rather than derived from the config. Any of these observed present in-sandbox
// is a leak. Extend as the allowlist model firms up.
var SensitiveEnv = []string{
	"ANTHROPIC_API_KEY",
	"GH_TOKEN", "GITHUB_TOKEN",
	"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	"SSH_AUTH_SOCK",
	"GPG_AGENT_INFO",
	"DBUS_SESSION_BUS_ADDRESS",
}

// Oracle holds the config-derived expected/denied sets for one run.
type Oracle struct {
	allowedDomains map[string]bool // expected-open egress
	injectDomains  map[string]bool // token expected here, nowhere else
	cdnHosts       map[string]bool // reachable but must be un-injected
	denyRead       []string        // sensitive paths that must be unreadable
	allowRead      []string        // read-only re-binds inside a deny region
	sensitiveEnv   map[string]bool
}

// NewOracle builds an oracle from rein's emitted sandbox config.
func NewOracle(cfg srt.Config) *Oracle {
	o := &Oracle{
		allowedDomains: lowerSet(cfg.Network.AllowedDomains),
		cdnHosts:       lowerSet(proxy.CDNHosts),
		sensitiveEnv:   lowerSet(SensitiveEnv),
	}
	if cfg.Network.MitmProxy != nil {
		o.injectDomains = lowerSet(cfg.Network.MitmProxy.Domains)
	} else {
		o.injectDomains = map[string]bool{}
	}
	for _, d := range cfg.Filesystem.DenyRead {
		o.denyRead = append(o.denyRead, filepath.Clean(d))
	}
	for _, a := range cfg.Filesystem.AllowRead {
		o.allowRead = append(o.allowRead, filepath.Clean(a))
	}
	return o
}

// Classify returns the oracle's verdict for one observation.
func (o *Oracle) Classify(obs Observation) Result {
	switch obs.Kind {
	case KindNetwork:
		return o.classifyNetwork(obs)
	case KindFile:
		return o.classifyFile(obs)
	case KindEnv:
		return o.classifyEnv(obs)
	default:
		return Result{obs, VerdictUnknown, "unrecognized observation kind"}
	}
}

// ClassifyAll classifies a batch, sorted leaks-first for review.
func (o *Oracle) ClassifyAll(obs []Observation) []Result {
	out := make([]Result, 0, len(obs))
	for _, ob := range obs {
		out = append(out, o.Classify(ob))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return verdictRank(out[i].Verdict) < verdictRank(out[j].Verdict)
	})
	return out
}

func (o *Oracle) classifyNetwork(obs Observation) Result {
	host := normHost(obs.Target)
	allowed := o.allowedDomains[host]
	inject := o.injectDomains[host]

	// Token placement is the sharpest leak: a token must appear IFF this is an
	// inject host. A token on a CDN/passthrough host leaks it onto a pre-signed
	// asset URL (hosts.go classPassthrough rationale).
	if obs.TokenInjected && !inject {
		reason := "rein token injected onto non-inject host " + host
		if o.cdnHosts[host] {
			reason = "rein token injected onto CDN host " + host + " (leaks onto a pre-signed asset URL)"
		}
		return Result{obs, VerdictLeak, reason}
	}
	if inject && obs.Reachable && !obs.TokenInjected {
		// Reachable inject host with no token would 401 — a functional break,
		// not a leak. Surface as regression.
		return Result{obs, VerdictRegression, "inject host reachable but no token observed"}
	}

	if allowed {
		if obs.Reachable {
			return Result{obs, VerdictOK, "expected-open egress reachable"}
		}
		return Result{obs, VerdictRegression, "expected-open host " + host + " unreachable in sandbox"}
	}
	// Not in the allowlist: must be denied.
	if obs.Reachable {
		return Result{obs, VerdictLeak, "denied host " + host + " reachable in sandbox (egress escape)"}
	}
	return Result{obs, VerdictOK, "denied host correctly unreachable"}
}

func (o *Oracle) classifyFile(obs Observation) Result {
	p := filepath.Clean(obs.Target)
	denyEntry, denyLen := deepestMatch(p, o.denyRead)
	allowEntry, allowLen := deepestMatch(p, o.allowRead)

	// srt applies deeper denies AFTER shallower allow-backs (config.go: shallow-
	// first sort + exact-match un-deny), so the MOST-SPECIFIC rule wins. An
	// allowRead re-bind strictly deeper than any covering denyRead re-exposes the
	// path READ-ONLY — expected-readable, not a leak (the #59 home-deny model's
	// normal shape). Build rejects an allowRead equal to/under an authoritative
	// cred deny, so a cred store can never be re-exposed this way.
	switch {
	case allowLen > denyLen:
		if obs.Reachable {
			return Result{obs, VerdictOK, "path re-exposed read-only via allowRead " + allowEntry}
		}
		return Result{obs, VerdictRegression, "allowRead path " + allowEntry + " unreadable in sandbox (agent needs it)"}
	case denyLen > 0:
		if obs.Reachable {
			return Result{obs, VerdictLeak, "sensitive path readable in sandbox (under denyRead " + denyEntry + ")"}
		}
		return Result{obs, VerdictOK, "sensitive path correctly unreadable"}
	default:
		// Covered by neither: the oracle can't judge intent from the flat config.
		// Report for triage rather than pass silently.
		if obs.Reachable {
			return Result{obs, VerdictUnknown, "readable path not covered by denyRead/allowRead — triage whether it should be denied"}
		}
		return Result{obs, VerdictUnknown, "unreadable path not covered by denyRead/allowRead"}
	}
}

func (o *Oracle) classifyEnv(obs Observation) Result {
	if o.sensitiveEnv[strings.ToLower(obs.Target)] {
		if obs.Reachable {
			return Result{obs, VerdictLeak, "sensitive env var " + obs.Target + " present in sandbox"}
		}
		return Result{obs, VerdictOK, "sensitive env var correctly scrubbed"}
	}
	return Result{obs, VerdictUnknown, "env var not in the sensitive denylist — triage"}
}

// deepestMatch returns the longest entry that covers p (p equals it or nests
// under it) and its length as a specificity measure — among ancestors of p, a
// deeper path is a longer cleaned string. Length 0 means no entry covers p.
func deepestMatch(p string, entries []string) (string, int) {
	best := ""
	bestLen := 0
	for _, e := range entries {
		if p == e || pathWithin(p, e) {
			if len(e) > bestLen {
				best, bestLen = e, len(e)
			}
		}
	}
	return best, bestLen
}

// HasLeak reports whether any result is a leak — the harness's fail condition.
func HasLeak(results []Result) bool {
	for _, r := range results {
		if r.Verdict == VerdictLeak {
			return true
		}
	}
	return false
}

func verdictRank(v Verdict) int {
	switch v {
	case VerdictLeak:
		return 0
	case VerdictRegression:
		return 1
	case VerdictUnknown:
		return 2
	default:
		return 3
	}
}

func lowerSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, s := range in {
		m[normHost(s)] = true
	}
	return m
}

func normHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// pathWithin reports whether child is nested under parent (both cleaned,
// segment-aware). Mirrors srt.pathWithin (unexported there).
func pathWithin(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
