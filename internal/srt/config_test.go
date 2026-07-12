package srt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildGoldenShape(t *testing.T) {
	cfg, err := Build(Params{
		SocketPath:  "/run/user/1000/rein/run-x/proxy.sock",
		WorkingTree: "/home/dev/work/repo",
		DenyReadCredStores: []string{
			"/home/dev/.config/gh",
			"/home/dev/.ssh",
			"/home/dev/.netrc",
		},
		RuntimeDenyRead: []string{"/run/user/1000/rein/run-x", "/run/user/1000"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got, err := cfg.MarshalIndent()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Round-trip and assert the load-bearing invariants rather than a brittle
	// byte-golden (denyRead order is sorted; a golden would churn on any path).
	var rt Config
	if err := json.Unmarshal(got, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// allowedDomains = exactly the 3 inject + 3 CDN hosts + the local
	// declare virtual host (issue #35).
	wantAllowed := map[string]bool{
		"github.com": true, "api.github.com": true, "uploads.github.com": true,
		"codeload.github.com": true, "objects.githubusercontent.com": true, "raw.githubusercontent.com": true,
		"declare.rein.internal": true,
	}
	if len(rt.Network.AllowedDomains) != len(wantAllowed) {
		t.Errorf("allowedDomains = %v, want 7 (3 inject + 3 cdn + declare host)", rt.Network.AllowedDomains)
	}
	for _, d := range rt.Network.AllowedDomains {
		if !wantAllowed[d] {
			t.Errorf("unexpected allowed domain %q", d)
		}
	}

	// mitmProxy.domains = EXACTLY the 3 inject hosts + the local declare
	// host (routed to the socket, answered locally) — no CDN, no wildcard.
	wantInject := map[string]bool{
		"github.com": true, "api.github.com": true, "uploads.github.com": true,
		"declare.rein.internal": true,
	}
	if rt.Network.MitmProxy == nil {
		t.Fatal("mitmProxy nil")
	}
	if len(rt.Network.MitmProxy.Domains) != 4 {
		t.Errorf("mitmProxy.domains = %v, want the 3 inject hosts + declare host", rt.Network.MitmProxy.Domains)
	}
	for _, d := range rt.Network.MitmProxy.Domains {
		if !wantInject[d] {
			t.Errorf("mitmProxy.domains includes non-inject host %q", d)
		}
		if strings.Contains(d, "*") {
			t.Errorf("mitmProxy.domains has wildcard %q", d)
		}
	}
	if rt.Network.MitmProxy.SocketPath != "/run/user/1000/rein/run-x/proxy.sock" {
		t.Errorf("socketPath = %q", rt.Network.MitmProxy.SocketPath)
	}

	// strictAllowlist true; deniedDomains empty (present, not null).
	if !rt.Network.StrictAllowlist {
		t.Error("strictAllowlist must be true")
	}
	if rt.Network.DeniedDomains == nil {
		t.Error("deniedDomains must marshal as [] not null")
	}

	// CDN hosts are allowed egress but NOT injected.
	for _, cdn := range []string{"codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com"} {
		for _, d := range rt.Network.MitmProxy.Domains {
			if d == cdn {
				t.Errorf("CDN host %q must not be injected", cdn)
			}
		}
	}

	// filesystem: working tree in allowWrite (not allowRead); denyRead covers
	// the credential stores; allowRead/denyWrite empty but present.
	if len(rt.Filesystem.AllowWrite) != 1 || rt.Filesystem.AllowWrite[0] != "/home/dev/work/repo" {
		t.Errorf("allowWrite = %v, want [working tree]", rt.Filesystem.AllowWrite)
	}
	if len(rt.Filesystem.AllowRead) != 0 {
		t.Errorf("allowRead must be empty (working tree goes in allowWrite), got %v", rt.Filesystem.AllowRead)
	}
	drSet := map[string]bool{}
	for _, d := range rt.Filesystem.DenyRead {
		drSet[d] = true
	}
	for _, want := range []string{"/home/dev/.config/gh", "/home/dev/.ssh", "/home/dev/.netrc", "/run/user/1000"} {
		if !drSet[want] {
			t.Errorf("denyRead missing %q; got %v", want, rt.Filesystem.DenyRead)
		}
	}
}

// TestBuildExtraAllowWrite asserts that an ExtraAllowWrite dir (the per-run agent
// scratch dir that becomes the sandboxed child's TMPDIR) is bound writable
// ALONGSIDE the working tree — the config-level half of the EROFS fix.
func TestBuildExtraAllowWrite(t *testing.T) {
	rt, err := Build(Params{
		SocketPath:      "/run/user/1000/rein/run-x/proxy.sock",
		WorkingTree:     "/home/dev/work/repo",
		ExtraAllowWrite: []string{"/tmp/rein-agent-tmp-abc"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	awSet := map[string]bool{}
	for _, w := range rt.Filesystem.AllowWrite {
		awSet[w] = true
	}
	for _, want := range []string{"/home/dev/work/repo", "/tmp/rein-agent-tmp-abc"} {
		if !awSet[want] {
			t.Errorf("allowWrite missing %q; got %v", want, rt.Filesystem.AllowWrite)
		}
	}
}

// TestBuildPinsWritableGitDirs asserts the rename-parent hardening: each passed
// `.git` dir is pinned as an allowWrite mountpoint AND its hooks/config land in
// denyWrite (so `mv .git` fails EBUSY and the hook/config surfaces stay ro even
// on worktrees srt's CWD-scoped scan misses).
func TestBuildPinsWritableGitDirs(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	wt := filepath.Join(root, "other")
	repoGit := filepath.Join(repo, ".git")
	wtGit := filepath.Join(wt, ".git")
	for _, d := range []string{repoGit, wtGit} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Build(Params{
		SocketPath:      filepath.Join(root, "run", "proxy.sock"),
		WorkingTree:     repo,
		ExtraAllowWrite: []string{wt},
		WritableGitDirs: []string{repoGit, wtGit},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	aw := map[string]bool{}
	for _, w := range cfg.Filesystem.AllowWrite {
		aw[w] = true
	}
	for _, want := range []string{repo, wt, repoGit, wtGit} {
		if !aw[want] {
			t.Errorf("allowWrite missing pinned path %q; got %v", want, cfg.Filesystem.AllowWrite)
		}
	}
	dw := map[string]bool{}
	for _, w := range cfg.Filesystem.DenyWrite {
		dw[w] = true
	}
	for _, g := range []string{repoGit, wtGit} {
		for _, sub := range []string{"hooks", "config", "config.worktree"} {
			want := filepath.Join(g, sub)
			if !dw[want] {
				t.Errorf("denyWrite missing %q; got %v", want, cfg.Filesystem.DenyWrite)
			}
		}
	}
}

// TestValidateRejectsDanglingDenyWrite asserts a hand-built config whose
// denyWrite is not under any allowWrite is rejected (it would be a no-op that
// silently drops the hooks/config protection).
func TestValidateRejectsDanglingDenyWrite(t *testing.T) {
	good, err := Build(Params{SocketPath: "/run/s.sock", WorkingTree: "/w"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	bad := good
	bad.Filesystem.DenyWrite = []string{"/elsewhere/.git/hooks"}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected Validate to reject denyWrite not under any allowWrite")
	}
	// A relative denyWrite entry is also rejected.
	bad2 := good
	bad2.Filesystem.DenyWrite = []string{"rel/.git/hooks"}
	if err := bad2.Validate(); err == nil {
		t.Fatal("expected Validate to reject a relative denyWrite entry")
	}
}

// TestBuildThreadsExtraDomainsEgressOnly asserts CP4.5's core invariant: extra
// egress domains land in allowedDomains (egress-allowed) but NEVER in
// mitmProxy.domains (never injected), and a duplicate/GitHub host dedupes.
func TestBuildThreadsExtraDomainsEgressOnly(t *testing.T) {
	cfg, err := Build(Params{
		SocketPath:  "/run/user/1000/rein/run-x/proxy.sock",
		WorkingTree: "/home/dev/work/repo",
		ExtraAllowedDomains: []string{
			"api.anthropic.com",
			"registry.npmjs.org",
			"*.internal.example.com",
			"github.com", // duplicate of an inject host -> must dedupe, not double-list
		},
	})
	if err != nil {
		t.Fatalf("Build with extra domains: %v", err)
	}

	// Each extra host is egress-allowed.
	for _, want := range []string{"api.anthropic.com", "registry.npmjs.org", "*.internal.example.com"} {
		if !contains(cfg.Network.AllowedDomains, want) {
			t.Errorf("extra domain %q missing from allowedDomains: %v", want, cfg.Network.AllowedDomains)
		}
	}
	// github.com appears exactly once (deduped against the inject host).
	var ghCount int
	for _, d := range cfg.Network.AllowedDomains {
		if d == "github.com" {
			ghCount++
		}
	}
	if ghCount != 1 {
		t.Errorf("github.com should appear once in allowedDomains, got %d: %v", ghCount, cfg.Network.AllowedDomains)
	}

	// NONE of the extra hosts may be injected — mitmProxy.domains stays EXACTLY
	// the three GitHub inject hosts + the local declare host.
	if len(cfg.Network.MitmProxy.Domains) != 4 {
		t.Fatalf("mitmProxy.domains must stay the 3 inject hosts + declare host, got %v", cfg.Network.MitmProxy.Domains)
	}
	for _, d := range cfg.Network.MitmProxy.Domains {
		if d == "api.anthropic.com" || d == "registry.npmjs.org" || strings.Contains(d, "*") {
			t.Errorf("extra/egress host %q leaked into mitmProxy.domains (would be injected!)", d)
		}
	}
}

// TestValidateRejectsAllowAllWildcard: a bare `*` (or empty) in allowedDomains
// defeats strictAllowlist and must be rejected; a strict *.suffix is fine.
func TestValidateRejectsAllowAllWildcard(t *testing.T) {
	good, err := Build(Params{SocketPath: "/run/s.sock", WorkingTree: "/w"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// *.suffix is accepted.
	okWild := deepCopy(t, good)
	okWild.Network.AllowedDomains = append(okWild.Network.AllowedDomains, "*.anthropic.com")
	if err := okWild.Validate(); err != nil {
		t.Errorf("Validate rejected a legal *.suffix in allowedDomains: %v", err)
	}
	// bare * / empty / wildcards covering a managed GitHub host are rejected.
	for _, bad := range []string{"*", "", "*.*", "*.github.com", "*.githubusercontent.com"} {
		c := deepCopy(t, good)
		c.Network.AllowedDomains = append(c.Network.AllowedDomains, bad)
		if err := c.Validate(); err == nil {
			t.Errorf("Validate accepted conflicting/over-broad allowedDomains entry %q", bad)
		}
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	if _, err := Build(Params{WorkingTree: "/x"}); err == nil {
		t.Error("Build allowed empty SocketPath")
	}
	if _, err := Build(Params{SocketPath: "/x/s.sock"}); err == nil {
		t.Error("Build allowed empty WorkingTree")
	}
}

func TestValidateCatchesWeakenings(t *testing.T) {
	good, err := Build(Params{SocketPath: "/run/s.sock", WorkingTree: "/w"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"strictAllowlist off", func(c *Config) { c.Network.StrictAllowlist = false }},
		{"nil mitmProxy", func(c *Config) { c.Network.MitmProxy = nil }},
		{"empty allowedDomains", func(c *Config) { c.Network.AllowedDomains = nil }},
		{"wildcard inject domain", func(c *Config) { c.Network.MitmProxy.Domains = []string{"*.github.com"} }},
		{"cdn in inject domains", func(c *Config) {
			c.Network.MitmProxy.Domains = append(c.Network.MitmProxy.Domains, "codeload.github.com")
			c.Network.AllowedDomains = append(c.Network.AllowedDomains, "codeload.github.com")
		}},
		{"empty allowWrite", func(c *Config) { c.Filesystem.AllowWrite = nil }},
		// allowWrite EQUAL to a denyRead stays rejected (contradiction; srt
		// would re-bind it writable). Strictly-under is legal now — see
		// TestValidateAllowReadContradictions.
		{"working tree equals denyRead", func(c *Config) {
			c.Filesystem.DenyRead = append(c.Filesystem.DenyRead, "/w")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := deepCopy(t, good)
			tt.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("Validate accepted a weakened config (%s)", tt.name)
			}
		})
	}
}

// TestBuildDenyHomeLayersWithCredDenies is the #59 core shape: the wholesale
// home deny is EMITTED ALONGSIDE the targeted credential denies (layered, not
// replacing — belt and suspenders), the allow-backs land in allowRead, and the
// working tree under the denied $HOME is accepted (srt 0.0.63 re-binds write
// paths under a deny tmpfs read-write — the old Validate rejection was
// factually wrong).
func TestBuildDenyHomeLayersWithCredDenies(t *testing.T) {
	cfg, err := Build(Params{
		SocketPath:  "/run/user/1000/rein/run-x/proxy.sock",
		WorkingTree: "/home/dev/work/repo", // under the home deny — must be legal
		DenyReadCredStores: []string{
			"/home/dev/.ssh",
			"/home/dev/.cargo/credentials.toml", // file deny inside an allowed-back dir
		},
		DenyReadHome: "/home/dev",
		AllowRead: []string{
			"/home/dev/.claude",
			"/home/dev/.cargo", // CONTAINS the credentials.toml deny — legal; srt keeps the file denied
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	dr := map[string]bool{}
	for _, d := range cfg.Filesystem.DenyRead {
		dr[d] = true
	}
	// Home deny AND the targeted denies are all present — layered.
	for _, want := range []string{"/home/dev", "/home/dev/.ssh", "/home/dev/.cargo/credentials.toml"} {
		if !dr[want] {
			t.Errorf("denyRead missing %q; got %v", want, cfg.Filesystem.DenyRead)
		}
	}
	ar := map[string]bool{}
	for _, a := range cfg.Filesystem.AllowRead {
		ar[a] = true
	}
	for _, want := range []string{"/home/dev/.claude", "/home/dev/.cargo"} {
		if !ar[want] {
			t.Errorf("allowRead missing %q; got %v", want, cfg.Filesystem.AllowRead)
		}
	}
	// The cred FILE deny survives the ~/.cargo dir allow-back in the emitted
	// config (srt's exact-match-only rule keeps it denied at runtime —
	// verified 0.0.63; this asserts rein didn't drop or "carve out" the entry).
	if !dr["/home/dev/.cargo/credentials.toml"] {
		t.Error("cargo credentials file deny was dropped when its parent dir is allowed back")
	}
	if cfg.Filesystem.AllowWrite[0] != "/home/dev/work/repo" {
		t.Errorf("working tree under home deny must stay allowWrite[0]; got %v", cfg.Filesystem.AllowWrite)
	}
}

// TestBuildRejectsWideningUnderAuthoritativeDeny: no widening path — allowRead
// allow-back, working tree, or extra write dir — may sit AT or UNDER a
// credential-store/runtime deny. srt would re-bind it over the deny tmpfs and
// re-expose exactly what the deny hides. Fail closed.
func TestBuildRejectsWideningUnderAuthoritativeDeny(t *testing.T) {
	base := Params{
		SocketPath:         "/run/user/1000/rein/run-x/proxy.sock",
		WorkingTree:        "/home/dev/work/repo",
		DenyReadCredStores: []string{"/home/dev/.config/gh", "/home/dev/.ssh"},
		RuntimeDenyRead:    []string{"/run/user/1000"},
		DenyReadHome:       "/home/dev",
	}
	cases := []struct {
		name   string
		mutate func(*Params)
	}{
		{"allowRead under cred deny", func(p *Params) { p.AllowRead = []string{"/home/dev/.config/gh/hosts"} }},
		{"allowRead equals cred deny", func(p *Params) { p.AllowRead = []string{"/home/dev/.ssh"} }},
		{"allowRead under runtime deny", func(p *Params) { p.AllowRead = []string{"/run/user/1000/keyring"} }},
		{"allowRead equals home deny (kill-switch bypass)", func(p *Params) { p.AllowRead = []string{"/home/dev"} }},
		{"allowRead contains home deny", func(p *Params) { p.AllowRead = []string{"/home"} }},
		{"working tree under cred deny", func(p *Params) { p.WorkingTree = "/home/dev/.ssh/repo" }},
		{"extra allowWrite under cred deny", func(p *Params) { p.ExtraAllowWrite = []string{"/home/dev/.config/gh/tmp"} }},
		{"relative allowRead", func(p *Params) { p.AllowRead = []string{".claude"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			if _, err := Build(p); err == nil {
				t.Errorf("Build accepted %s", tc.name)
			}
		})
	}

	// Control: the unmutated base with a legal allow-back builds fine.
	p := base
	p.AllowRead = []string{"/home/dev/.claude"}
	if _, err := Build(p); err != nil {
		t.Errorf("control Build failed: %v", err)
	}
}

// TestBuildResolvesSymlinkedWideningPaths is the D6 (#44) regression: a
// SYMLINKED working tree / allow-back / extra write dir pointing into a
// credential deny must be rejected — the overlap check runs on the resolved
// target, not the alias. Uses real dirs+symlinks because EvalSymlinks needs
// them on disk.
func TestBuildResolvesSymlinkedWideningPaths(t *testing.T) {
	base := t.TempDir()
	cred := filepath.Join(base, "secrets")
	inner := filepath.Join(cred, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "innocent-link")
	if err := os.Symlink(inner, link); err != nil {
		t.Fatal(err)
	}

	params := func() Params {
		return Params{
			SocketPath:         "/run/user/1000/rein/run-x/proxy.sock",
			WorkingTree:        filepath.Join(base, "work"),
			DenyReadCredStores: []string{cred},
		}
	}

	p := params()
	p.WorkingTree = link
	if _, err := Build(p); err == nil {
		t.Error("Build accepted a symlinked working tree resolving into a credential deny (D6)")
	}

	p = params()
	p.DenyReadHome = base
	p.AllowRead = []string{link}
	if _, err := Build(p); err == nil {
		t.Error("Build accepted a symlinked allowRead resolving into a credential deny (D6)")
	}

	p = params()
	p.ExtraAllowWrite = []string{link}
	if _, err := Build(p); err == nil {
		t.Error("Build accepted a symlinked ExtraAllowWrite resolving into a credential deny (D6)")
	}

	// Control: a symlink to a benign dir is resolved and accepted, and the
	// RESOLVED form is what lands in the emitted config.
	benign := filepath.Join(base, "benign")
	if err := os.MkdirAll(benign, 0o700); err != nil {
		t.Fatal(err)
	}
	benignLink := filepath.Join(base, "benign-link")
	if err := os.Symlink(benign, benignLink); err != nil {
		t.Fatal(err)
	}
	p = params()
	p.WorkingTree = benignLink
	cfg, err := Build(p)
	if err != nil {
		t.Fatalf("Build rejected a benign symlinked working tree: %v", err)
	}
	// /tmp itself may be a symlink on some hosts; compare against the resolved benign path.
	wantTree, err := filepath.EvalSymlinks(benign)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Filesystem.AllowWrite[0] != wantTree {
		t.Errorf("emitted working tree %q is not the symlink-resolved %q", cfg.Filesystem.AllowWrite[0], wantTree)
	}
}

// TestBuildDanglingSymlinkWideningRejected (#59 security review, S2): a
// widening entry that is a DANGLING symlink is rejected hard — its meaning
// would change the moment someone recreates the target — while a PLAIN-ABSENT
// path stays tolerated (the curated set adds ~/.pyenv etc. unconditionally
// and srt skips absent paths at mount time; a box without pyenv must not
// brick).
func TestBuildDanglingSymlinkWideningRejected(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir()) // /tmp may itself be a symlink
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "gone"), dangling); err != nil {
		t.Fatal(err)
	}

	params := func() Params {
		return Params{
			SocketPath:   "/run/user/1000/rein/run-x/proxy.sock",
			WorkingTree:  work,
			DenyReadHome: base,
		}
	}

	// Dangling symlink: rejected in every widening position.
	p := params()
	p.AllowRead = []string{dangling}
	if _, err := Build(p); err == nil {
		t.Error("Build accepted a dangling-symlink allowRead")
	}
	p = params()
	p.WorkingTree = dangling
	if _, err := Build(p); err == nil {
		t.Error("Build accepted a dangling-symlink working tree")
	}
	p = params()
	p.ExtraAllowWrite = []string{dangling}
	if _, err := Build(p); err == nil {
		t.Error("Build accepted a dangling-symlink ExtraAllowWrite")
	}

	// Plain-absent allowRead: tolerated (emitted; srt skips it at mount).
	absent := filepath.Join(base, ".pyenv") // never created
	p = params()
	p.AllowRead = []string{absent}
	cfg, err := Build(p)
	if err != nil {
		t.Fatalf("Build rejected a plain-absent allowRead: %v", err)
	}
	found := false
	for _, a := range cfg.Filesystem.AllowRead {
		if a == absent {
			found = true
		}
	}
	if !found {
		t.Errorf("plain-absent allowRead %q not emitted; got %v", absent, cfg.Filesystem.AllowRead)
	}
}

// TestValidateAllowReadContradictions: the structural backstop — an allowRead
// entry equal to a denyRead entry, or a relative allowRead, is rejected even
// in a hand-built config that bypassed Build.
func TestValidateAllowReadContradictions(t *testing.T) {
	good, err := Build(Params{SocketPath: "/run/s.sock", WorkingTree: "/w"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	c := deepCopy(t, good)
	c.Filesystem.DenyRead = append(c.Filesystem.DenyRead, "/home/dev/.ssh")
	c.Filesystem.AllowRead = append(c.Filesystem.AllowRead, "/home/dev/.ssh")
	if err := c.Validate(); err == nil {
		t.Error("Validate accepted allowRead == denyRead (would un-hide the denied path)")
	}

	c = deepCopy(t, good)
	c.Filesystem.AllowRead = append(c.Filesystem.AllowRead, "relative/path")
	if err := c.Validate(); err == nil {
		t.Error("Validate accepted a relative allowRead entry")
	}

	// allowWrite strictly UNDER a denyRead is now LEGAL (srt 0.0.63 re-binds
	// it rw; the #59 working-tree-under-$HOME shape). Equality stays rejected
	// (covered by TestValidateCatchesWeakenings).
	c = deepCopy(t, good)
	c.Filesystem.DenyRead = append(c.Filesystem.DenyRead, "/home/dev")
	c.Filesystem.AllowWrite = []string{"/home/dev/work/repo"}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate rejected allowWrite under denyRead — legal on srt 0.0.63 (write re-bind): %v", err)
	}
}

func deepCopy(t *testing.T, c Config) Config {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
