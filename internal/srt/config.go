// Package srt builds and validates the per-run srt (Anthropic sandbox-runtime)
// settings.json, assembles the scrubbed exec environment, runs the launch
// preflight, and proves — by actually launching srt with a probe — that the
// two srt fail-open traps (config-null-fallback and missing-seccomp) did not
// silently disarm rein's protections. See cp3-srt-0063-schema.md for the
// authoritative 0.0.63 schema and the six gaps rein must close outside srt.
//
// Design stance: never trust that the settings file rein wrote took effect.
// rein (a) emits the config from a TYPED struct and Validate()s it before
// writing, and (b) treats the RUNNING srt as ground truth via
// VerifyConfigApplied. The typed struct only avoids hand-rolled-JSON mistakes;
// the verify step is the guarantee.
//
// Verified on 0.0.63 (schema-spec discrepancy, cli.js:121-129): the spec warns
// that loadConfig returns null on a missing/malformed settings file and cli
// falls back to getDefaultConfig() with an EMPTY denyRead — a fail-open. That
// fallback only fires when NO --settings flag is passed. rein ALWAYS passes
// `-s <settings>`, and on that path srt EXITS 1 ("Refusing to run with the
// default config") on any load failure rather than falling open. So the empty-
// denyRead fail-open is NOT reachable on rein's path in 0.0.63 — but rein keeps
// VerifyConfigApplied anyway: it guards against that behavior changing in a
// future srt, proves denyRead SEMANTICS (file => /dev/null) hold for this
// version, and catches srt running the probe unsandboxed. srt's own exit-1 is
// itself caught (VerifyConfigApplied sees a non-ProbeOK code => fail closed).
package srt

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TomHennen/rein/internal/proxy"
)

// Config is the typed srt 0.0.63 settings.json. Only the fields rein sets are
// modeled; srt "strip"s unknown top-level keys, and every field we omit takes
// srt's schema default. JSON tags match the schema exactly. We deliberately do
// NOT model credentials/tlsTerminate/allowAllUnixSockets/filesystem.disabled —
// setting any of those would weaken the sandbox (see the schema spec), so they
// are unreachable from this struct by construction.
type Config struct {
	Network    Network    `json:"network"`
	Filesystem Filesystem `json:"filesystem"`
}

// Network is srt's network stanza. allowedDomains is the egress allowlist
// (empty => block all); deniedDomains is checked first (deny wins);
// strictAllowlist true makes an unmatched host a hard deny (never an ask);
// mitmProxy routes exactly its domains to rein's socket.
type Network struct {
	AllowedDomains  []string   `json:"allowedDomains"`
	DeniedDomains   []string   `json:"deniedDomains"`
	StrictAllowlist bool       `json:"strictAllowlist"`
	MitmProxy       *MitmProxy `json:"mitmProxy,omitempty"`
}

// MitmProxy points srt at rein's per-run unix socket and enumerates the EXACT
// hosts to route there. domains must be exact inject hosts, never a wildcard
// (gap #6) — a wildcard would pull CDN hosts into the injector and leak the
// token onto a pre-signed asset URL.
type MitmProxy struct {
	SocketPath string   `json:"socketPath"`
	Domains    []string `json:"domains"`
}

// Filesystem is srt's filesystem stanza. Read is deny-then-allow-back
// (denyRead => tmpfs/dev-null over the path; allowRead re-binds read-only).
// Write is allow-only (root ro-bind; allowWrite => rw bind). The working tree
// goes in allowWrite, NOT allowRead: under the broad denyRead only allowWrite
// paths are re-bound read-WRITE, and git needs to write .git/index, do
// checkouts, and commit (schema-spec BLOCKER).
type Filesystem struct {
	DenyRead   []string `json:"denyRead"`
	AllowRead  []string `json:"allowRead"`
	AllowWrite []string `json:"allowWrite"`
	DenyWrite  []string `json:"denyWrite"`
}

// Params are the per-run inputs to Build. All paths should be absolute; Build
// cleans them but does not resolve symlinks (the placement check in
// internal/proxy owns symlink resolution for the socket).
type Params struct {
	// SocketPath is rein's per-run proxy socket. It MUST sit outside every
	// srt bind-mount (the caller placement-checks it); denyRead also hides its
	// parent runtime dir in-sandbox for defense-in-depth.
	SocketPath string

	// WorkingTree is the agent's repo checkout — the single allowWrite path.
	WorkingTree string

	// ExtraAllowedDomains are additional egress hosts the operator opted into
	// (the wrapped agent's own API, npm, PyPI, …). They are appended to srt's
	// allowedDomains — egress-allowed but NEVER injected: they are deliberately
	// kept OUT of mitmProxy.domains, so each gets a direct end-to-end TLS tunnel
	// to itself (no rein token), validated against the system roots in the CA
	// bundle. The caller resolves + merges these from the built-in default, the
	// session's allow_domains, and REIN_ALLOW_DOMAINS (see ResolveExtraAllowedDomains).
	ExtraAllowedDomains []string

	// ExtraAllowWrite are additional writable dirs the caller opted into
	// (e.g. a scratch/output dir). Each becomes an allowWrite bind AND a
	// forbidden dir for the socket placement check.
	ExtraAllowWrite []string

	// DenyReadCredStores are the ambient credential stores + rein's own key
	// dir + audit log to hide from the agent. The caller resolves these to
	// absolute paths (missing paths are harmless — denyRead of an absent path
	// is a no-op in srt).
	DenyReadCredStores []string

	// RuntimeDir is the per-run socket's parent (e.g. $XDG_RUNTIME_DIR/rein/<id>)
	// and the /run/user/<uid> tree — both denyRead'd so the agent can't reach
	// the socket file or other users' runtime sockets from inside the sandbox.
	RuntimeDenyRead []string

	// SentinelPath, when non-empty, is added to denyRead. VerifyConfigApplied
	// uses it to prove the deny-read actually took effect (content-compare):
	// the sentinel is a real file with known bytes OUTSIDE the sandbox; if the
	// agent-side read returns those bytes, srt's config did NOT apply and rein
	// fails closed. Empty in the config handed to the real agent launch is
	// fine, but the launch path always sets it for the verify spawn.
	SentinelPath string
}

// Build assembles a validated Config from p. It returns an error if the result
// would fail Validate — so a caller can rely on Build's output being safe to
// write. Host lists come from internal/proxy (single source of truth): the six
// allowed domains (3 inject + 3 CDN) and the exactly-three mitmProxy domains.
func Build(p Params) (Config, error) {
	if p.SocketPath == "" {
		return Config{}, fmt.Errorf("srt: SocketPath is required")
	}
	if p.WorkingTree == "" {
		return Config{}, fmt.Errorf("srt: WorkingTree is required")
	}
	// All paths MUST be absolute. A relative path would make pathWithin (used by
	// Validate's working-tree-under-denyRead guard and the socket placement
	// check) compare apples to oranges and silently skip the guard — a
	// fail-open seam. Reject up front rather than clean a relative path into a
	// CWD-relative absolute one the caller didn't intend.
	for _, group := range [][]string{
		{p.SocketPath, p.WorkingTree, p.SentinelPath},
		p.ExtraAllowWrite, p.DenyReadCredStores, p.RuntimeDenyRead,
	} {
		for _, path := range group {
			if path != "" && !filepath.IsAbs(path) {
				return Config{}, fmt.Errorf("srt: path %q must be absolute", path)
			}
		}
	}

	// allowedDomains = the GitHub inject + CDN hosts, plus the operator's extra
	// egress hosts. The extras go here ONLY (never into mitmProxy.domains below),
	// so a non-GitHub host is passed through to its real endpoint uninjected.
	// Lowercase + dedupe so an extra domain that duplicates an inject/CDN host
	// (e.g. someone lists github.com) collapses instead of double-listing — a
	// duplicate can never create an injection gap (injection is driven by the
	// exact mitmProxy.domains, not the allowlist), so dedupe, don't error.
	allowed := make([]string, 0, len(proxy.InjectHosts)+len(proxy.CDNHosts)+len(p.ExtraAllowedDomains))
	allowed = append(allowed, proxy.InjectHosts...)
	allowed = append(allowed, proxy.CDNHosts...)
	allowed = append(allowed, p.ExtraAllowedDomains...)
	allowed = dedupeLowerKeepOrder(allowed)

	allowWrite := make([]string, 0, 1+len(p.ExtraAllowWrite))
	allowWrite = append(allowWrite, filepath.Clean(p.WorkingTree))
	for _, d := range p.ExtraAllowWrite {
		allowWrite = append(allowWrite, filepath.Clean(d))
	}

	denyRead := make([]string, 0, len(p.DenyReadCredStores)+len(p.RuntimeDenyRead)+1)
	denyRead = append(denyRead, cleanAll(p.DenyReadCredStores)...)
	denyRead = append(denyRead, cleanAll(p.RuntimeDenyRead)...)
	if p.SentinelPath != "" {
		denyRead = append(denyRead, filepath.Clean(p.SentinelPath))
	}
	denyRead = dedupeSorted(denyRead)

	cfg := Config{
		Network: Network{
			AllowedDomains:  append([]string(nil), allowed...),
			DeniedDomains:   []string{},
			StrictAllowlist: true,
			MitmProxy: &MitmProxy{
				SocketPath: filepath.Clean(p.SocketPath),
				// EXACTLY the inject hosts — never CDN, never a wildcard.
				Domains: append([]string(nil), proxy.InjectHosts...),
			},
		},
		Filesystem: Filesystem{
			DenyRead:   denyRead,
			AllowRead:  []string{},
			AllowWrite: allowWrite,
			DenyWrite:  []string{},
		},
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate is the fail-closed sanity check on an emitted Config. It catches the
// mistakes that would silently weaken the sandbox even when srt accepts the
// file: a missing/loosened mitmProxy, a wildcard or CDN host in the injector,
// an inject host not actually allowed egress, or a working tree missing from
// allowWrite. It does NOT re-check srt's own schema — that is srt's job, and
// VerifyConfigApplied is the backstop for srt accepting-but-not-applying.
func (c Config) Validate() error {
	n := c.Network
	if len(n.AllowedDomains) == 0 {
		return fmt.Errorf("srt: allowedDomains is empty (srt would block all egress)")
	}
	if !n.StrictAllowlist {
		return fmt.Errorf("srt: strictAllowlist must be true (an unmatched host must hard-deny, never ask)")
	}
	if n.MitmProxy == nil {
		return fmt.Errorf("srt: mitmProxy is nil (no injection path; credentials would never reach GitHub)")
	}
	if n.MitmProxy.SocketPath == "" {
		return fmt.Errorf("srt: mitmProxy.socketPath is empty")
	}
	if len(n.MitmProxy.Domains) == 0 {
		return fmt.Errorf("srt: mitmProxy.domains is empty (nothing would be injected)")
	}
	// Guard the allowlist itself against an over-broad wildcard that would defeat
	// strictAllowlist. Exact hosts and strict `*.suffix` wildcards are fine (extra
	// egress domains may legitimately be wildcards — the caller WARNs on those);
	// a bare `*` (or an empty entry) would allow ALL egress and is rejected.
	for _, d := range n.AllowedDomains {
		dl := strings.ToLower(strings.TrimSpace(d))
		if dl == "" {
			return fmt.Errorf("srt: allowedDomains contains an empty entry")
		}
		if strings.Contains(dl, "*") && (dl == "*" || !strings.HasPrefix(dl, "*.") || strings.Count(dl, "*") != 1) {
			return fmt.Errorf("srt: allowedDomains entry %q is an over-broad wildcard (only exact hosts or *.suffix are permitted; a bare * would allow all egress)", d)
		}
	}
	allowedSet := toSet(n.AllowedDomains)
	cdnSet := toSet(proxy.CDNHosts)
	for _, d := range n.MitmProxy.Domains {
		if strings.ContainsAny(d, "*") {
			return fmt.Errorf("srt: mitmProxy.domains contains wildcard %q (gap #6: exact inject hosts only, or a CDN host gets injected)", d)
		}
		if !allowedSet[strings.ToLower(d)] {
			return fmt.Errorf("srt: mitmProxy domain %q is not in allowedDomains (it must pass egress to be injectable)", d)
		}
		if cdnSet[strings.ToLower(d)] {
			return fmt.Errorf("srt: mitmProxy.domains includes CDN host %q (CDN hosts must never be injected — token leak onto pre-signed URLs)", d)
		}
	}
	// Every inject host must be BOTH allowed egress and routed to the socket,
	// else a git/api op silently bypasses injection and 401s (or worse, egress
	// is denied and the op just fails).
	injectSet := toSet(n.MitmProxy.Domains)
	for _, h := range proxy.InjectHosts {
		if !allowedSet[strings.ToLower(h)] {
			return fmt.Errorf("srt: inject host %q missing from allowedDomains", h)
		}
		if !injectSet[strings.ToLower(h)] {
			return fmt.Errorf("srt: inject host %q missing from mitmProxy.domains", h)
		}
	}
	if len(c.Filesystem.AllowWrite) == 0 {
		return fmt.Errorf("srt: allowWrite is empty (the working tree must be writable or git ops fail under the denyRead tmpfs)")
	}
	// The working tree (allowWrite[0]) must NOT sit under any denyRead path, or
	// srt tmpfs's it out from under the rw bind.
	for _, dr := range c.Filesystem.DenyRead {
		for _, aw := range c.Filesystem.AllowWrite {
			if pathWithin(aw, dr) {
				return fmt.Errorf("srt: allowWrite %q is under denyRead %q (the working tree would be tmpfs'd)", aw, dr)
			}
		}
	}
	return nil
}

// MarshalIndent renders the config as the settings.json bytes to write to disk.
func (c Config) MarshalIndent() ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}

func cleanAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p == "" {
			continue
		}
		out = append(out, filepath.Clean(p))
	}
	return out
}

// dedupeLowerKeepOrder lowercases + trims each entry (dropping a trailing FQDN
// dot and empties) and removes duplicates, preserving first-occurrence order so
// the GitHub inject/CDN hosts stay ahead of the operator's extras.
func dedupeLowerKeepOrder(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, d := range in {
		k := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(d), ".")))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

func dedupeSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func toSet(in []string) map[string]bool {
	s := make(map[string]bool, len(in))
	for _, v := range in {
		s[strings.ToLower(v)] = true
	}
	return s
}

// pathWithin reports whether child equals parent or is nested under it. Both
// are cleaned before comparison; segment-aware so "/a/bc" is not within "/a/b".
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
