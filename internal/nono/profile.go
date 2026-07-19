// Package nono generates the nono sandbox profile rein hands to
// `nono run --profile`. The profile IS the security boundary: it routes the
// agent's GitHub egress through rein's loopback proxy (TLS-terminate + token
// inject + declare tap), hides credentials, and isolates the approval channel.
// One source of truth — cmd/rein/run_nono.go writes Build's output each launch.
//
// Schema is nono 0.68.0, verified in docs/design-nono-profile-schema.md. The
// PROVISIONAL struct in design-nono-pivot.md §2.2 is SUPERSEDED by that doc:
// the profile is NESTED (network/filesystem/linux/environment/groups),
// upstream_proxy is a bare host:port string, deny_credentials is a policy GROUP
// (not a path list), env injection is environment.set_vars, and there is NO
// working upstream-proxy auth in 0.68.0 (external_proxy.auth is unimplemented —
// do not emit it).
package nono

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/TomHennen/rein/internal/proxy"
)

// caEnvVars are the env vars that point the agent's tools at rein's CA PEM so
// they trust rein's re-signed certs. Hardcoded here (with agreement to migrate
// to internal/sandboxutil.CAEnvVars once PR #140 lands — that is the eventual
// single source of truth). Injected via environment.set_vars; nono passes them
// through verbatim (unlike HTTP(S)_PROXY/NO_PROXY, which nono owns and overrides).
var caEnvVars = []string{
	"SSL_CERT_FILE",
	"GIT_SSL_CAINFO",
	"NODE_EXTRA_CA_CERTS",
	"CURL_CA_BUNDLE",
}

// baselineGitConfig is the git config rein must set for the two-hop proxy to
// work (findings: docs/nono-git-push-spike-findings.md):
//   - http.proxyAuthMethod=basic — nono's own agent→nono hop uses a
//     `nono:<token>` basic credential; git defaults to negotiate and 407s.
//   - http.postBuffer — large chunked pushes hang without a raised buffer.
//
// Emitted through GIT_CONFIG_COUNT/KEY_n/VALUE_n so it composes with the agent's
// own (read-only) config instead of clobbering GIT_CONFIG_GLOBAL.
var baselineGitConfig = []GitConfig{
	{Key: "http.proxyAuthMethod", Value: "basic"},
	{Key: "http.postBuffer", Value: "524288000"},
	// core.excludesFile has a COMPILE-TIME default (~/.config/git/ignore) that git
	// reads even with an empty global config. Under nono's default-deny fs that
	// read returns EACCES — FATAL to `git add`/`status` ("cannot use … as an
	// exclude file") — unlike the tolerated ENOENT srt's tmpfs $HOME produces.
	// Pin it to /dev/null so git reads an empty exclude set instead of the denied
	// path. Same rationale as the GIT_CONFIG_GLOBAL/SYSTEM redirect.
	{Key: "core.excludesFile", Value: "/dev/null"},
}

// GitConfig is one git config key/value pair injected via GIT_CONFIG_*.
type GitConfig struct {
	Key   string
	Value string
}

// Params carries the per-run, runtime-variable inputs to Build. The security
// host lists (inject / CDN / declare) are NOT here — Build reads them straight
// from internal/proxy so the profile and the proxy can never drift.
type Params struct {
	// ListenAddr is rein's loopback proxy as a BARE host:port (e.g.
	// "127.0.0.1:47821"). It becomes network.upstream_proxy. NO scheme — nono
	// dials it with TcpStream::connect; "http://..." fails to dial.
	ListenAddr string

	// CACertPath is the absolute path to rein's CA PEM. It must live in an
	// agent-READABLE directory (granted via filesystem.read_file) so the
	// sandboxed tools can trust rein's re-signed certs. Keep it separate from
	// any secret dir — filesystem is default-deny, so granting this file leaks
	// nothing else.
	CACertPath string

	// ExtraDomains are operator opt-in egress hosts (e.g. api.anthropic.com,
	// npm). They go into allow_domain ONLY — never injected, and (per the
	// authoritative schema doc) NOT into upstream_bypass: a design-faithful
	// profile routes them THROUGH rein as an opaque CONNECT tunnel so rein
	// still sees the egress. See the P1 §3c dependency note in Build.
	ExtraDomains []string

	// UnixSockets are AF_UNIX pathname sockets the sandboxed agent legitimately
	// needs, granted back under af_unix_mediation:"pathname". Default EMPTY.
	// NEVER add the host tmux/approval socket: the sandboxed agent must be
	// unable to connect() it and self-approve (the host popup runs outside the
	// sandbox and is unaffected).
	UnixSockets []string

	// ExtraGitConfig is appended after baselineGitConfig, for callers that need
	// additional git config. Optional.
	ExtraGitConfig []GitConfig

	// ClaudeConfigDir, when non-empty, is delivered as CLAUDE_CONFIG_DIR — the
	// rein-owned, agent-WRITABLE claude config overlay (issue #94). Host ~/.claude
	// / ~/.claude.json stay hidden by nono's default-deny fs (nothing grants them);
	// claude is repointed here so a real agent can still run. Must be an absolute
	// path (it is also granted agent-WRITABLE via a nono --allow). Optional — a run
	// that never launches claude leaves it empty and CLAUDE_CONFIG_DIR unset.
	ClaudeConfigDir string

	// Name / Description populate meta. Optional; sensible defaults applied.
	Name        string
	Description string
}

// Profile is the nono 0.68.0 profile, nested per the real schema.
type Profile struct {
	Schema      string      `json:"$schema,omitempty"`
	Meta        *Meta       `json:"meta,omitempty"`
	Groups      Groups      `json:"groups"`
	Network     Network     `json:"network"`
	Linux       Linux       `json:"linux"`
	Filesystem  Filesystem  `json:"filesystem"`
	Environment Environment `json:"environment"`
}

type Meta struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// Groups selects nono policy groups. deny_credentials is a fixed, required
// policy group that blocks known credential locations — NOT a path list.
type Groups struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude,omitempty"`
}

// Network routes egress. upstream_proxy is the rein loopback listener; every
// allow_domain host except those in upstream_bypass tunnels through it.
type Network struct {
	Block          bool     `json:"block"`                     // false — true is incompatible with proxy mode
	AllowDomain    []string `json:"allow_domain"`              // Inject ∪ CDN ∪ Declare ∪ Extra
	UpstreamProxy  string   `json:"upstream_proxy,omitempty"`  // bare host:port, NO scheme, NO auth
	UpstreamBypass []string `json:"upstream_bypass,omitempty"` // CDN hosts verbatim → direct (never rein)
	// No auth field exists in 0.68.0. external_proxy.auth is rejected at
	// validate and unimplemented — do not add it.
}

// Linux carries Linux-only controls. af_unix_mediation:"pathname" is the
// approval-channel isolation control (deny-by-default AF_UNIX pathname mediation).
type Linux struct {
	AfUnixMediation string `json:"af_unix_mediation,omitempty"`
}

// Filesystem is default-deny on Linux; only read_file/read_dir grants matter
// (filesystem.deny is a Linux no-op). UnixSocket is the connect-only allowlist
// under af_unix_mediation. NOTE: nono's `profile schema` OMITS unix_socket* even
// though the binary accepts them (schema drift, per the schema doc) — trust the
// guide/sample, which is what this field mirrors.
type Filesystem struct {
	ReadFile   []string `json:"read_file,omitempty"`
	UnixSocket []string `json:"unix_socket"`
}

// Environment injects arbitrary env after nono's env filtering. PATH, NONO_*,
// and HTTP(S)_PROXY/NO_PROXY are nono-managed and must NOT be set here.
type Environment struct {
	SetVars map[string]string `json:"set_vars,omitempty"`
}

const (
	schemaURL          = "https://nono.sh/schema/profile.json"
	defaultName        = "rein-sandbox"
	defaultDescription = "rein credential-broker sandbox profile (generated)."
	afUnixMediation    = "pathname"
	denyCredentials    = "deny_credentials"
)

// includedGroups are the policy groups rein always includes. deny_credentials
// is load-bearing (invariant 3); the shell groups are defense-in-depth.
var includedGroups = []string{denyCredentials, "deny_shell_history", "deny_shell_configs"}

// Build assembles the nono profile from p and the proxy package's host lists,
// enforcing the six security invariants and failing CLOSED on any violation
// rather than emitting a permissive profile. Invariants (each load-bearing):
//
//  1. GitHub → rein: every inject host + DeclareHost tunnels through
//     upstream_proxy (rein's listener) — NOT in upstream_bypass.
//  2. CDN → direct: CDN hosts in upstream_bypass so no token lands on a CDN.
//  3. Credentials hidden: deny_credentials group included.
//  4. Approval-channel isolation: af_unix_mediation:"pathname", no tmux socket
//     grant (UnixSockets defaults empty).
//  5. Loopback isolation is nono's job: a sandboxed connect() to rein's port is
//     denied by nono while nono's own proxy works — so no proxy-auth is needed
//     (and none is wireable in 0.68.0). We rely on that; nothing to emit.
//  6. CA trust: CA PEM granted via filesystem.read_file and pointed at by the
//     four CA env vars.
func Build(p Params) (Profile, error) {
	// Fail closed on missing security inputs.
	if strings.TrimSpace(p.ListenAddr) == "" {
		return Profile{}, fmt.Errorf("nono: ListenAddr is empty (no upstream_proxy ⇒ GitHub egress would not reach rein)")
	}
	if strings.Contains(p.ListenAddr, "://") {
		return Profile{}, fmt.Errorf("nono: ListenAddr %q has a scheme; upstream_proxy is a bare host:port (nono dials it directly)", p.ListenAddr)
	}
	if _, _, err := splitHostPort(p.ListenAddr); err != nil {
		return Profile{}, fmt.Errorf("nono: ListenAddr %q is not a host:port: %w", p.ListenAddr, err)
	}
	if strings.TrimSpace(p.CACertPath) == "" {
		return Profile{}, fmt.Errorf("nono: CACertPath is empty (agent tools could not trust rein's CA ⇒ every TLS op fails)")
	}
	if !filepath.IsAbs(p.CACertPath) {
		return Profile{}, fmt.Errorf("nono: CACertPath %q must be absolute (nono grants and env-points to a concrete path)", p.CACertPath)
	}

	// allow_domain = Inject ∪ CDN ∪ Declare ∪ Extra (dedupe, lowercase). A
	// duplicate can never open an injection gap (injection is keyed off the
	// proxy's exact host classifier, not this list), so dedupe rather than error.
	allow := make([]string, 0, len(proxy.InjectHosts)+len(proxy.CDNHosts)+len(proxy.LocalHosts)+len(p.ExtraDomains))
	allow = append(allow, proxy.InjectHosts...)
	allow = append(allow, proxy.CDNHosts...)
	allow = append(allow, proxy.LocalHosts...)
	for _, d := range p.ExtraDomains {
		if strings.TrimSpace(d) != "" {
			allow = append(allow, d)
		}
	}
	allow = dedupeLower(allow)

	// upstream_bypass = CDNHosts VERBATIM (invariant 2). Inject + Declare hosts
	// must reach rein, so they are never here. ExtraDomains are deliberately
	// NOT bypassed — see the P1 §3c dependency note below.
	bypass := dedupeLower(append([]string(nil), proxy.CDNHosts...))

	// CA trust env (invariant 6) + baseline git config.
	setVars := make(map[string]string, len(caEnvVars)+8)
	for _, k := range caEnvVars {
		setVars[k] = p.CACertPath
	}
	// Redirect git's global + system config to /dev/null so git never opens the
	// developer's ~/.gitconfig (or /etc/gitconfig). Under nono's default-deny fs
	// that read returns EACCES, which git treats as a FATAL config error — not the
	// tolerated ENOENT — so WITHOUT this every in-sandbox git op dies before it
	// runs ("unknown error occurred while reading the configuration files"). The
	// nono equivalent of srt's GIT_CONFIG_GLOBAL/SYSTEM redirect (env.go). The
	// non-impersonating identity still comes from the GIT_CONFIG_* entries below,
	// which layer ON TOP of these empty files.
	setVars["GIT_CONFIG_GLOBAL"] = "/dev/null"
	setVars["GIT_CONFIG_SYSTEM"] = "/dev/null"
	gitCfg := append(append([]GitConfig(nil), baselineGitConfig...), p.ExtraGitConfig...)
	applyGitConfig(setVars, gitCfg)

	// CLAUDE_CONFIG_DIR overlay (#94): repoint claude at the rein-owned writable
	// config dir. Host ~/.claude stays hidden by nono's default-deny fs; this env
	// override is what lets a real claude agent run in-sandbox. Emit only when set,
	// so a non-claude run leaves the profile (and its golden) unchanged.
	if ccd := strings.TrimSpace(p.ClaudeConfigDir); ccd != "" {
		if !filepath.IsAbs(ccd) {
			return Profile{}, fmt.Errorf("nono: ClaudeConfigDir %q must be absolute (it is granted --allow and set as CLAUDE_CONFIG_DIR)", ccd)
		}
		setVars["CLAUDE_CONFIG_DIR"] = ccd
	}

	name := p.Name
	if name == "" {
		name = defaultName
	}
	desc := p.Description
	if desc == "" {
		desc = defaultDescription
	}

	pr := Profile{
		Schema: schemaURL,
		Meta:   &Meta{Name: name, Description: desc},
		Groups: Groups{Include: append([]string(nil), includedGroups...)},
		Network: Network{
			Block:          false,
			AllowDomain:    allow,
			UpstreamProxy:  p.ListenAddr,
			UpstreamBypass: bypass,
		},
		Linux: Linux{AfUnixMediation: afUnixMediation},
		Filesystem: Filesystem{
			ReadFile:   []string{p.CACertPath},
			UnixSocket: normalizeSockets(p.UnixSockets),
		},
		Environment: Environment{SetVars: setVars},
	}

	if err := pr.Validate(); err != nil {
		return Profile{}, err
	}
	return pr, nil
}

// Validate is the fail-closed sanity check on a built Profile — it catches the
// mistakes that would silently weaken the sandbox even when nono accepts the
// file (a missing deny_credentials group, mediation dropped by an omitempty
// footgun, an inject host that bypasses rein, a CDN host injected, no CA grant).
func (pr Profile) Validate() error {
	// Invariant 1: GitHub egress must route to rein.
	if strings.TrimSpace(pr.Network.UpstreamProxy) == "" {
		return fmt.Errorf("nono: network.upstream_proxy is empty (GitHub egress would not reach rein)")
	}
	// Re-check the shape Build enforced, so a caller who repoints upstream_proxy
	// to a URL/garbage after Build still fails the gate. (The target *address*
	// being rein's is inherently uncheckable here — nono doesn't know rein's addr.)
	if strings.Contains(pr.Network.UpstreamProxy, "://") {
		return fmt.Errorf("nono: network.upstream_proxy %q has a scheme; it must be a bare host:port", pr.Network.UpstreamProxy)
	}
	if _, _, err := splitHostPort(pr.Network.UpstreamProxy); err != nil {
		return fmt.Errorf("nono: network.upstream_proxy %q is not a host:port: %w", pr.Network.UpstreamProxy, err)
	}
	if pr.Network.Block {
		return fmt.Errorf("nono: network.block is true (incompatible with proxy mode; all egress blocked)")
	}
	if len(pr.Network.AllowDomain) == 0 {
		return fmt.Errorf("nono: network.allow_domain is empty (nono would block all egress)")
	}
	allowSet := toSet(pr.Network.AllowDomain)
	bypassSet := toSet(pr.Network.UpstreamBypass)

	// Every inject host must be allowed egress AND must NOT be bypassed — else
	// its traffic silently skips rein (no injection ⇒ 401, or token leak).
	for _, h := range proxy.InjectHosts {
		hl := lower(h)
		if !allowSet[hl] {
			return fmt.Errorf("nono: inject host %q missing from allow_domain", h)
		}
		if bypassSet[hl] {
			return fmt.Errorf("nono: inject host %q is in upstream_bypass (it must tunnel through rein, not go direct)", h)
		}
	}
	// The declare virtual host must reach rein (answered locally by the proxy);
	// bypassing it would break in-sandbox `rein declare`.
	for _, h := range proxy.LocalHosts {
		hl := lower(h)
		if !allowSet[hl] {
			return fmt.Errorf("nono: local host %q missing from allow_domain", h)
		}
		if bypassSet[hl] {
			return fmt.Errorf("nono: local host %q is in upstream_bypass (it must reach rein to be answered locally)", h)
		}
	}
	// Invariant 2: every CDN host must bypass rein (direct TLS with GitHub's
	// real cert) so no rein-injected token ever lands on a pre-signed CDN URL.
	for _, h := range proxy.CDNHosts {
		hl := lower(h)
		if !allowSet[hl] {
			return fmt.Errorf("nono: CDN host %q missing from allow_domain", h)
		}
		if !bypassSet[hl] {
			return fmt.Errorf("nono: CDN host %q missing from upstream_bypass (a token could be injected onto a CDN request)", h)
		}
	}
	// upstream_bypass must be EXACTLY the CDN list — no extras. An extra exact
	// host would silently route operator egress direct (rein loses visibility);
	// worse, a `*.` wildcard entry (nono honors these in bypass) could SHADOW an
	// inject host — e.g. "*.github.com" matches api/uploads.github.com and routes
	// them direct, skipping rein. The exact-match inject check above cannot catch
	// a wildcard, so bound bypass to the CDN set here (subset ∩ superset ⇒ equal).
	cdnSet := toSet(proxy.CDNHosts)
	for _, h := range pr.Network.UpstreamBypass {
		if !cdnSet[lower(h)] {
			return fmt.Errorf("nono: upstream_bypass entry %q is not a CDN host (bypass must be exactly the CDN list; an extra or wildcard entry could route an inject host direct, skipping rein)", h)
		}
	}

	// Invariant 3: credentials hidden.
	if !contains(pr.Groups.Include, denyCredentials) {
		return fmt.Errorf("nono: groups.include is missing %q (the agent could read the App key / gh token / ssh keys)", denyCredentials)
	}

	// Invariant 4: approval-channel isolation. The omitempty footgun means an
	// empty value would drop the field entirely — assert it explicitly.
	if pr.Linux.AfUnixMediation != afUnixMediation {
		return fmt.Errorf("nono: linux.af_unix_mediation is %q, want %q (the agent could connect() the host approval socket and self-approve)", pr.Linux.AfUnixMediation, afUnixMediation)
	}

	// Invariant 6: CA trust — the PEM must be granted read and every CA env var
	// must point at it. (Invariant 5, loopback isolation, is nono's own
	// mediation and has no profile field — see Build's doc comment.)
	if len(pr.Filesystem.ReadFile) == 0 {
		return fmt.Errorf("nono: filesystem.read_file is empty (the CA PEM would be unreadable under default-deny fs ⇒ every TLS op fails)")
	}
	caPath := pr.Filesystem.ReadFile[0]
	if strings.TrimSpace(caPath) == "" {
		return fmt.Errorf("nono: filesystem.read_file[0] (CA PEM) is empty")
	}
	for _, k := range caEnvVars {
		if pr.Environment.SetVars[k] != caPath {
			return fmt.Errorf("nono: environment.set_vars[%q] is %q, want the CA path %q (tools would not trust rein's CA)", k, pr.Environment.SetVars[k], caPath)
		}
	}
	return nil
}

// MarshalIndent renders the profile as indented JSON with a trailing newline
// (golden-file stable). Marshaled from typed structs — never string-concatenated.
func (pr Profile) MarshalIndent() ([]byte, error) {
	b, err := json.MarshalIndent(pr, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// applyGitConfig writes GIT_CONFIG_COUNT + GIT_CONFIG_KEY_n/VALUE_n into setVars.
// Managing the indices here keeps callers from hand-counting (a miscount silently
// drops config). No-op for an empty list.
func applyGitConfig(setVars map[string]string, cfg []GitConfig) {
	if len(cfg) == 0 {
		return
	}
	setVars["GIT_CONFIG_COUNT"] = strconv.Itoa(len(cfg))
	for i, c := range cfg {
		setVars["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = c.Key
		setVars["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = c.Value
	}
}

// normalizeSockets trims and drops empties; returns a non-nil empty slice so the
// golden JSON always shows "unix_socket": [] (explicit default-none), never null.
func normalizeSockets(in []string) []string {
	out := []string{}
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func splitHostPort(addr string) (host, port string, err error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("missing port")
	}
	host, port = addr[:i], addr[i+1:]
	if port == "" {
		return "", "", fmt.Errorf("empty port")
	}
	if _, e := strconv.Atoi(strings.TrimSpace(port)); e != nil {
		return "", "", fmt.Errorf("port %q is not numeric", port)
	}
	if strings.TrimSpace(host) == "" {
		return "", "", fmt.Errorf("empty host")
	}
	return host, port, nil
}

func dedupeLower(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		l := lower(s)
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

func lower(s string) string { return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(s, "."))) }

func toSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, s := range in {
		m[lower(s)] = true
	}
	return m
}

func contains(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}
