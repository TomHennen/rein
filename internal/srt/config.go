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
	"os"
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

// Params are the per-run inputs to Build. All paths must be absolute. Build
// cleans every path and SYMLINK-RESOLVES the widening paths (WorkingTree,
// ExtraAllowWrite, AllowRead, DenyReadHome) before the overlap checks, so a
// symlinked path cannot smuggle a widening under a credential deny (audit
// finding D6, issue #44). The placement check in internal/proxy still owns
// symlink resolution for the socket.
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

	// RuntimeDenyRead is the per-run socket's parent (e.g. $XDG_RUNTIME_DIR/rein/<id>)
	// and the /run/user/<uid> tree — both denyRead'd so the agent can't reach
	// the socket file or other users' runtime sockets from inside the sandbox.
	RuntimeDenyRead []string

	// DenyReadHome, when non-empty, is the developer's home directory to
	// deny-read WHOLESALE (issue #59: the phase1-design §4.2 model). It layers
	// WITH — never replaces — DenyReadCredStores: the targeted credential
	// denials stay authoritative belt-and-suspenders even under the broad home
	// deny. srt 0.0.63 renders a dir deny as an empty writable tmpfs, re-binds
	// allowWrite paths under it read-write (verified: linux-sandbox-utils.js
	// pushReadDenyDirMounts), and ro-binds AllowRead paths back on top. Empty
	// means "do not hide $HOME" — the REIN_SANDBOX_SHOW_HOME kill switch; the
	// caller must warn LOUDLY when it takes that path.
	DenyReadHome string

	// AllowRead are the allow-back paths re-exposed READ-ONLY under
	// DenyReadHome (srt allowRead == allowWithinDeny): the agent's install
	// chain, its config dir, and a curated toolchain set, plus the operator's
	// REIN_SANDBOX_ALLOW_READ extras. Build symlink-resolves each entry and
	// REJECTS any that would re-expose a DenyReadCredStores / RuntimeDenyRead /
	// SentinelPath entry (fail closed): srt would ro-bind an allow-back inside
	// a credential deny right back over its tmpfs. An allow-back that merely
	// CONTAINS a deeper credential deny is fine — srt applies deeper denies
	// after shallower allow-backs (shallow-first sort + exact-match-only file
	// un-deny, both verified on 0.0.63), so the deny stays authoritative.
	AllowRead []string

	// WritableGitDirs are the `.git` DIRECTORIES of the writable checkouts (the
	// autodetected working tree plus every mapped worktree, issue #64) for which
	// `.git` is a real directory. Build PINS each one as its own allowWrite
	// mountpoint and adds each one's `hooks` dir and `config` file to denyWrite.
	//
	// Why pin, not just deny hooks/config: srt already ro-binds `.git/hooks`
	// (always) and `.git/config` (default) as a mandatory measure, but that
	// protection is PATH-based and does NOT pin `.git` itself — so a prompt-
	// injected agent defeats it by RENAMING the parent: `mv .git .git.aside`
	// makes the ro-bind mountpoints follow the rename, freeing the `.git` NAME
	// in the still-writable working tree, after which the agent rebuilds a fresh,
	// fully-valid `.git` (cp -a the aside + a malicious `hooks/pre-commit`) that
	// then runs AS THE DEVELOPER, ON THE HOST, at their next git command — a
	// time-displaced sandbox escape. Verified against real srt: the rename
	// defeats srt's built-in deny; pinning `.git` (an allowWrite --bind onto
	// itself) turns it into a mountpoint whose rename fails EBUSY, closing it.
	//
	// Why rein emits the hooks/config deny too instead of leaning on srt's: srt's
	// mandatory deny scans only srt's CWD subtree (depth 3), so the OTHER #64
	// worktrees — bound at arbitrary paths outside CWD — are NOT covered by it.
	// rein lists each checkout's hooks/config in denyWrite explicitly so the
	// protection is independent of srt's CWD-scoped scan and of srt internals.
	//
	// SCOPE (documented residual gaps, tracked as follow-ups): this covers each
	// checkout's TOP-LEVEL `.git` only. It does NOT yet pin submodule gitdirs
	// (`.git/modules/*`, each with its own writable hooks/config — a confirmed
	// unprotected surface) nor linked worktrees whose `.git` is a FILE (surfaces
	// live in the common gitdir, possibly outside the bound tree). The caller
	// only passes `.git` paths that are real directories; the file case is
	// skipped rather than mis-bound.
	WritableGitDirs []string

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
		{p.SocketPath, p.WorkingTree, p.SentinelPath, p.DenyReadHome},
		p.ExtraAllowWrite, p.DenyReadCredStores, p.RuntimeDenyRead, p.AllowRead,
		p.WritableGitDirs,
	} {
		for _, path := range group {
			if path != "" && !filepath.IsAbs(path) {
				return Config{}, fmt.Errorf("srt: path %q must be absolute", path)
			}
		}
	}

	// Symlink-resolve every WIDENING path (working tree, extra write dirs,
	// read allow-backs, and the home deny they are anchored under) BEFORE any
	// overlap check (audit finding D6, issue #44): a symlinked
	// REIN_SANDBOX_WORKDIR or allow-back pointing into a credential store must
	// be compared — and emitted — in resolved form, or the check runs on the
	// alias while srt binds the target. The DENY side is resolved into a
	// parallel COMPARISON-ONLY set (a symlinked $HOME, e.g. /home -> /var/home,
	// must not desynchronize the two sides); what is EMITTED into denyRead is
	// the caller's original cred/runtime/sentinel paths (srt itself resolves
	// file-deny destinations, and rewriting a dir deny to its target could
	// miss an alias srt would have covered) plus the RESOLVED homeDeny (the
	// tmpfs must land on the real home, and the allow-backs are anchored
	// under the resolved form).
	workTree, err := resolveWidening(p.WorkingTree)
	if err != nil {
		return Config{}, err
	}
	extraWrite := make([]string, 0, len(p.ExtraAllowWrite))
	for _, d := range p.ExtraAllowWrite {
		r, err := resolveWidening(d)
		if err != nil {
			return Config{}, err
		}
		extraWrite = append(extraWrite, r)
	}
	allowRead := make([]string, 0, len(p.AllowRead))
	for _, d := range p.AllowRead {
		r, err := resolveWidening(d)
		if err != nil {
			return Config{}, err
		}
		allowRead = append(allowRead, r)
	}
	homeDeny := ""
	if p.DenyReadHome != "" {
		if homeDeny, err = resolveWidening(p.DenyReadHome); err != nil {
			return Config{}, err
		}
	}

	// Authoritative denials (design §4.2 / phase1-design §4.2): no widening —
	// write bind or read allow-back — may sit AT or UNDER a credential-store,
	// runtime-socket, or sentinel deny path. srt would re-bind it over the deny
	// tmpfs (rw for allowWrite, ro for allowRead; verified on 0.0.63), silently
	// re-exposing exactly what the deny exists to hide. Fail closed. Sitting
	// under DenyReadHome is the INTENDED shape (that's the whole model), which
	// is why the home deny is a separate Params field and not folded into
	// DenyReadCredStores. Compare in symlink-resolved form on both sides.
	authoritative := make([]string, 0, len(p.DenyReadCredStores)+len(p.RuntimeDenyRead)+1)
	authoritative = append(authoritative, p.DenyReadCredStores...)
	authoritative = append(authoritative, p.RuntimeDenyRead...)
	if p.SentinelPath != "" {
		authoritative = append(authoritative, p.SentinelPath)
	}
	resolvedAuthoritative := make([]string, 0, len(authoritative))
	for _, d := range authoritative {
		r, rerr := proxy.ResolveAbs(d)
		if rerr != nil {
			r = filepath.Clean(d) // unresolvable stays forbidden by prefix — fail closed
		}
		resolvedAuthoritative = append(resolvedAuthoritative, r)
	}
	for _, w := range append(append([]string{workTree}, extraWrite...), allowRead...) {
		for _, d := range resolvedAuthoritative {
			if pathWithin(w, d) {
				return Config{}, fmt.Errorf("srt: widening path %q is at or under authoritative deny-read path %q and would re-expose it (fail closed; design §4.2)", w, d)
			}
		}
	}
	// An allow-back that equals or CONTAINS the home deny would un-hide $HOME
	// wholesale — the silent equivalent of the kill switch. Refuse: disabling
	// the home deny must go through the LOUD, banner-warned
	// REIN_SANDBOX_SHOW_HOME=1 path, never through an allowlist entry.
	if homeDeny != "" {
		for _, a := range allowRead {
			if pathWithin(homeDeny, a) {
				return Config{}, fmt.Errorf("srt: allowRead %q covers the whole home deny %q; to expose $HOME use the explicit REIN_SANDBOX_SHOW_HOME=1 kill switch instead", a, homeDeny)
			}
		}
	}

	// allowedDomains = the GitHub inject + CDN hosts, the local-only rein
	// virtual hosts (issue #35: declare.rein.internal must pass srt egress
	// matching to be routed at all), plus the operator's extra egress
	// hosts. The extras go into the allowlist ONLY (never into
	// mitmProxy.domains below), so a non-GitHub host is passed through to
	// its real endpoint uninjected. Lowercase + dedupe so an extra domain
	// that duplicates an inject/CDN host (e.g. someone lists github.com)
	// collapses instead of double-listing — a duplicate can never create
	// an injection gap (injection is driven by the exact
	// mitmProxy.domains, not the allowlist), so dedupe, don't error.
	allowed := make([]string, 0, len(proxy.InjectHosts)+len(proxy.CDNHosts)+len(proxy.LocalHosts)+len(p.ExtraAllowedDomains))
	allowed = append(allowed, proxy.InjectHosts...)
	allowed = append(allowed, proxy.CDNHosts...)
	allowed = append(allowed, proxy.LocalHosts...)
	allowed = append(allowed, p.ExtraAllowedDomains...)
	allowed = dedupeLowerKeepOrder(allowed)

	// Pin each writable checkout's `.git` as its own allowWrite mountpoint (see
	// Params.WritableGitDirs) and deny-write its hooks/ + config so the rename-
	// parent escape fails EBUSY. Symlink-resolve each like any widening path.
	// The pins sit UNDER the working-tree allowWrite (nested allowWrite is fine
	// in srt — verified 0.0.63); denyWrite ro-binds are emitted after allowWrite
	// binds (srt linux-sandbox-utils "Emitting denyWrite last …"), so hooks/config
	// land read-only on top of the pinned, writable `.git`.
	gitPins := make([]string, 0, len(p.WritableGitDirs))
	gitDeny := make([]string, 0, 3*len(p.WritableGitDirs))
	for _, g := range p.WritableGitDirs {
		r, rerr := resolveWidening(g)
		if rerr != nil {
			return Config{}, rerr
		}
		gitPins = append(gitPins, r)
		// hooks/ + config are the always-present exec surfaces. config.worktree
		// is the per-worktree config git ALSO reads when extensions.worktreeConfig
		// is set in the (read-only) common config — a writable exec surface
		// (core.pager/fsmonitor/…) that fires at the REPO ROOT. The agent cannot
		// enable the extension itself (it lives in the ro common config), so this
		// only bites repos already using `git config --worktree`/sparse-checkout —
		// but deny it UNCONDITIONALLY: harmless when absent (srt blocks its
		// creation), protective when present. Note it is a denyWrite of a path
		// under the pinned .git, so Validate's "denyWrite must be under an
		// allowWrite" check is satisfied even when the file does not yet exist.
		gitDeny = append(gitDeny,
			filepath.Join(r, "hooks"),
			filepath.Join(r, "config"),
			filepath.Join(r, "config.worktree"),
		)
	}

	allowWrite := make([]string, 0, 1+len(extraWrite)+len(gitPins))
	allowWrite = append(allowWrite, workTree)
	allowWrite = append(allowWrite, extraWrite...)
	allowWrite = append(allowWrite, gitPins...)

	denyRead := make([]string, 0, len(p.DenyReadCredStores)+len(p.RuntimeDenyRead)+2)
	denyRead = append(denyRead, cleanAll(p.DenyReadCredStores)...)
	denyRead = append(denyRead, cleanAll(p.RuntimeDenyRead)...)
	if p.SentinelPath != "" {
		denyRead = append(denyRead, filepath.Clean(p.SentinelPath))
	}
	if homeDeny != "" {
		denyRead = append(denyRead, homeDeny)
	}
	denyRead = dedupeSorted(denyRead)

	cfg := Config{
		Network: Network{
			AllowedDomains:  append([]string(nil), allowed...),
			DeniedDomains:   []string{},
			StrictAllowlist: true,
			MitmProxy: &MitmProxy{
				SocketPath: filepath.Clean(p.SocketPath),
				// EXACTLY the inject hosts + the local-only virtual hosts
				// (declare.rein.internal routes to rein's socket like the
				// inject hosts but is answered locally, never relayed) —
				// never CDN, never a wildcard.
				Domains: append(append([]string(nil), proxy.InjectHosts...), proxy.LocalHosts...),
			},
		},
		Filesystem: Filesystem{
			DenyRead:   denyRead,
			AllowRead:  dedupeSorted(allowRead),
			AllowWrite: allowWrite,
			DenyWrite:  dedupeSorted(gitDeny),
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
		// An extra-egress wildcard must not COVER a GitHub inject/CDN host: that
		// overlaps the exact hosts rein manages and (depending on srt's
		// allowlist-vs-mitmProxy precedence) could shadow injection routing. Exact
		// duplicates are harmless (deduped in Build); an overlapping wildcard is a
		// conflict and is rejected (extra domains must not conflict with the inject
		// hosts). e.g. *.github.com covers api.github.com; reject it.
		if strings.HasPrefix(dl, "*.") {
			suffix := dl[2:]
			managed := append(append([]string{}, proxy.InjectHosts...), proxy.CDNHosts...)
			managed = append(managed, proxy.LocalHosts...)
			for _, h := range managed {
				hl := strings.ToLower(h)
				if hl == suffix || strings.HasSuffix(hl, "."+suffix) {
					return fmt.Errorf("srt: allowedDomains wildcard %q overlaps managed host %q (extra egress domains must not conflict with the inject/CDN/local hosts)", d, h)
				}
			}
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
	// Same for the local-only virtual hosts (issue #35): a missing
	// declare.rein.internal route would silently break `rein declare`
	// in-sandbox — the whole write path would be locked with no way to
	// unlock it.
	for _, h := range proxy.LocalHosts {
		if !allowedSet[strings.ToLower(h)] {
			return fmt.Errorf("srt: local host %q missing from allowedDomains", h)
		}
		if !injectSet[strings.ToLower(h)] {
			return fmt.Errorf("srt: local host %q missing from mitmProxy.domains", h)
		}
	}
	if len(c.Filesystem.AllowWrite) == 0 {
		return fmt.Errorf("srt: allowWrite is empty (the working tree must be writable or git ops fail under the denyRead tmpfs)")
	}
	// allowWrite STRICTLY UNDER a denyRead is legal — indeed it is the #59
	// model's normal shape (working tree under the wholesale $HOME deny).
	// Verified on srt 0.0.63 (linux-sandbox-utils.js pushReadDenyDirMounts): a
	// write bind wiped by an ancestor denyRead tmpfs is automatically re-bound
	// read-write, so the earlier "would be tmpfs'd" rejection here was
	// factually wrong. The security question that remains — is the covering
	// deny the intended home deny or a credential store the re-bind would
	// re-expose? — cannot be answered from the flat config, so Build enforces
	// it with Params knowledge (widening-under-authoritative-deny fails
	// closed). What stays rejected here, both structurally nonsensical:
	//   - allowWrite EQUAL to a denyRead: "hide X" and "let the agent write X"
	//     is a contradiction, and srt's re-bind would resolve it in favor of
	//     writable — the weakening direction. Fail closed.
	//   - denyRead strictly under an allowWrite: a hidden path nested in the
	//     writable tree indicates a mis-scoped working dir (e.g.
	//     REIN_SANDBOX_WORKDIR=$HOME swallowing the cred stores). srt 0.0.63
	//     happens to apply the deny tmpfs after the write bind, so it is not
	//     literally re-exposed — but rein rejects the shape anyway rather than
	//     lean on that ordering.
	for _, dr := range c.Filesystem.DenyRead {
		for _, aw := range c.Filesystem.AllowWrite {
			if filepath.Clean(aw) == filepath.Clean(dr) {
				return fmt.Errorf("srt: allowWrite %q equals denyRead %q (contradictory: srt would re-bind it writable, un-hiding it)", aw, dr)
			}
			if pathWithin(dr, aw) {
				return fmt.Errorf("srt: denyRead %q sits under allowWrite %q (a hidden path nested in the writable tree is a mis-scoped working dir)", dr, aw)
			}
		}
	}
	// allowRead entries are re-bound read-only INSIDE deny tmpfs regions. An
	// entry EQUAL to a denyRead entry un-hides it outright: srt re-binds an
	// allowPath equal to the denied dir over its own tmpfs, and an exact
	// allowRead match makes srt SKIP a file deny entirely (verified 0.0.63).
	// Build's param-aware check already rejects allow-backs at/under the
	// authoritative denies; this structural backstop catches a hand-built
	// config expressing the same contradiction, and keeps every entry
	// absolute so pathWithin comparisons stay apples-to-apples.
	for _, ar := range c.Filesystem.AllowRead {
		if !filepath.IsAbs(ar) {
			return fmt.Errorf("srt: allowRead entry %q must be absolute", ar)
		}
		for _, dr := range c.Filesystem.DenyRead {
			if filepath.Clean(ar) == filepath.Clean(dr) {
				return fmt.Errorf("srt: allowRead %q equals denyRead %q (contradictory: srt would un-hide the denied path)", ar, dr)
			}
		}
	}
	// denyWrite entries (the pinned checkouts' hooks/config) are ro-binds layered
	// on top of the write binds; each must be absolute (pathWithin comparisons and
	// srt's own bind must be apples-to-apples) and must sit UNDER an allowWrite
	// path — a denyWithinAllow that is not within any allowWrite is a no-op that
	// silently drops the hook/config protection. Fail closed.
	for _, dw := range c.Filesystem.DenyWrite {
		if !filepath.IsAbs(dw) {
			return fmt.Errorf("srt: denyWrite entry %q must be absolute", dw)
		}
		within := false
		for _, aw := range c.Filesystem.AllowWrite {
			if pathWithin(filepath.Clean(dw), aw) {
				within = true
				break
			}
		}
		if !within {
			return fmt.Errorf("srt: denyWrite %q is not under any allowWrite path (it would be a no-op, silently dropping the hooks/config protection)", dw)
		}
	}
	return nil
}

// MarshalIndent renders the config as the settings.json bytes to write to disk.
func (c Config) MarshalIndent() ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}

// resolveWidening cleans and symlink-resolves a widening path (working tree,
// extra allowWrite, allowRead, home deny) via the shared placement-check
// primitive. Resolution failure fails closed: a widening path we cannot pin
// down must not be emitted into the config.
//
// A DANGLING SYMLINK is rejected outright (#59 security review): lstat sees a
// symlink but its target is gone, so ResolveAbs would fall back to resolving
// only the ancestors and emit the alias path with an unresolved leaf — a
// config entry whose meaning changes the moment someone recreates the target.
// Refuse to guess. A PLAIN-ABSENT path (lstat fails) stays tolerated exactly
// as before: curated allow-backs like ~/.pyenv are added unconditionally and
// srt skips absent paths at mount time — a box without pyenv must not brick.
func resolveWidening(p string) (string, error) {
	clean := filepath.Clean(p)
	if fi, lerr := os.Lstat(clean); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if _, serr := filepath.EvalSymlinks(clean); serr != nil {
			return "", fmt.Errorf("srt: widening path %q is a dangling symlink (%v); refusing to guess its target — remove the entry or fix the link", p, serr)
		}
	}
	r, err := proxy.ResolveAbs(clean)
	if err != nil {
		return "", fmt.Errorf("srt: resolve widening path %q: %w", p, err)
	}
	return r, nil
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
