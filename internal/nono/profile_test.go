package nono

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/proxy"
)

var update = flag.Bool("update", false, "regenerate golden files")

// goldenParams are the fixed inputs behind testdata/profile.golden.json. Keep
// them deterministic — the golden is human-reviewed; drift = red = re-review.
func goldenParams() Params {
	return Params{
		ListenAddr:  "127.0.0.1:47821",
		CACertPath:  "/home/user/.config/rein/ca/rein-ca.pem",
		Name:        "rein-sandbox",
		Description: "rein credential-broker sandbox profile (generated).",
	}
}

func mustBuild(t *testing.T, p Params) Profile {
	t.Helper()
	pr, err := Build(p)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return pr
}

func TestGolden(t *testing.T) {
	pr := mustBuild(t, goldenParams())
	got, err := pr.MarshalIndent()
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	golden := filepath.Join("testdata", "profile.golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run `go test -run TestGolden -update`): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("golden mismatch; run `go test ./internal/nono -run TestGolden -update` to regenerate\n--- got ---\n%s", got)
	}
}

// --- Security invariant structural assertions ---

func TestInvariant_GitHubHostsRouteToRein(t *testing.T) {
	pr := mustBuild(t, goldenParams())
	if pr.Network.UpstreamProxy != "127.0.0.1:47821" {
		t.Errorf("upstream_proxy = %q, want the rein listener", pr.Network.UpstreamProxy)
	}
	allow := toSet(pr.Network.AllowDomain)
	bypass := toSet(pr.Network.UpstreamBypass)
	for _, h := range proxy.InjectHosts {
		if !allow[lower(h)] {
			t.Errorf("inject host %q not in allow_domain", h)
		}
		if bypass[lower(h)] {
			t.Errorf("inject host %q in upstream_bypass (would skip rein)", h)
		}
	}
	for _, h := range proxy.LocalHosts {
		if bypass[lower(h)] {
			t.Errorf("declare host %q in upstream_bypass (would not reach rein)", h)
		}
	}
}

func TestInvariant_CDNHostsBypass(t *testing.T) {
	pr := mustBuild(t, goldenParams())
	bypass := toSet(pr.Network.UpstreamBypass)
	allow := toSet(pr.Network.AllowDomain)
	for _, h := range proxy.CDNHosts {
		if !bypass[lower(h)] {
			t.Errorf("CDN host %q not in upstream_bypass (token could be injected onto CDN)", h)
		}
		if !allow[lower(h)] {
			t.Errorf("CDN host %q not in allow_domain", h)
		}
	}
	// upstream_bypass must be EXACTLY the CDN hosts (verbatim) — no extras.
	if len(pr.Network.UpstreamBypass) != len(proxy.CDNHosts) {
		t.Errorf("upstream_bypass = %v, want exactly the %d CDN hosts", pr.Network.UpstreamBypass, len(proxy.CDNHosts))
	}
}

func TestInvariant_DenyCredentialsGroup(t *testing.T) {
	pr := mustBuild(t, goldenParams())
	if !contains(pr.Groups.Include, "deny_credentials") {
		t.Errorf("groups.include = %v, missing deny_credentials", pr.Groups.Include)
	}
}

func TestInvariant_AfUnixMediationPathname(t *testing.T) {
	pr := mustBuild(t, goldenParams())
	if pr.Linux.AfUnixMediation != "pathname" {
		t.Errorf("af_unix_mediation = %q, want pathname", pr.Linux.AfUnixMediation)
	}
	// Approval-channel isolation: default is NO unix socket allowlist (the agent
	// must not be able to connect the host tmux/approval socket).
	if len(pr.Filesystem.UnixSocket) != 0 {
		t.Errorf("unix_socket = %v, want empty by default", pr.Filesystem.UnixSocket)
	}
}

func TestInvariant_CATrust(t *testing.T) {
	pr := mustBuild(t, goldenParams())
	ca := "/home/user/.config/rein/ca/rein-ca.pem"
	if len(pr.Filesystem.ReadFile) != 1 || pr.Filesystem.ReadFile[0] != ca {
		t.Errorf("filesystem.read_file = %v, want [%q]", pr.Filesystem.ReadFile, ca)
	}
	for _, k := range caEnvVars {
		if pr.Environment.SetVars[k] != ca {
			t.Errorf("set_vars[%q] = %q, want %q", k, pr.Environment.SetVars[k], ca)
		}
	}
}

func TestExtraDomains_AllowOnly_NotBypassed(t *testing.T) {
	p := goldenParams()
	p.ExtraDomains = []string{"api.anthropic.com"}
	pr := mustBuild(t, p)
	if !toSet(pr.Network.AllowDomain)["api.anthropic.com"] {
		t.Errorf("extra domain not in allow_domain")
	}
	if toSet(pr.Network.UpstreamBypass)["api.anthropic.com"] {
		t.Errorf("extra domain must NOT be in upstream_bypass (routes through rein per schema doc)")
	}
}

func TestGitConfigWiring(t *testing.T) {
	p := goldenParams()
	p.ExtraGitConfig = []GitConfig{{Key: "http.version", Value: "HTTP/1.1"}}
	pr := mustBuild(t, p)
	sv := pr.Environment.SetVars
	// 3 baseline entries (proxyAuthMethod, postBuffer, core.excludesFile) + 1 extra.
	if sv["GIT_CONFIG_COUNT"] != "4" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 4", sv["GIT_CONFIG_COUNT"])
	}
	if sv["GIT_CONFIG_KEY_0"] != "http.proxyAuthMethod" || sv["GIT_CONFIG_VALUE_0"] != "basic" {
		t.Errorf("baseline proxyAuthMethod pair wrong: %q=%q", sv["GIT_CONFIG_KEY_0"], sv["GIT_CONFIG_VALUE_0"])
	}
	if sv["GIT_CONFIG_KEY_2"] != "core.excludesFile" || sv["GIT_CONFIG_VALUE_2"] != "/dev/null" {
		t.Errorf("baseline excludesFile pair wrong: %q=%q", sv["GIT_CONFIG_KEY_2"], sv["GIT_CONFIG_VALUE_2"])
	}
	// The extra config lands AFTER the baseline entries (index 3).
	if sv["GIT_CONFIG_KEY_3"] != "http.version" {
		t.Errorf("extra git config not appended: %q", sv["GIT_CONFIG_KEY_3"])
	}
}

// --- Fail-closed assertions ---

func TestFailClosed(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Params)
		wantS string
	}{
		{"empty listen addr", func(p *Params) { p.ListenAddr = "" }, "ListenAddr is empty"},
		{"listen addr with scheme", func(p *Params) { p.ListenAddr = "http://127.0.0.1:47821" }, "scheme"},
		{"listen addr no port", func(p *Params) { p.ListenAddr = "127.0.0.1" }, "host:port"},
		{"empty ca path", func(p *Params) { p.CACertPath = "" }, "CACertPath is empty"},
		{"relative ca path", func(p *Params) { p.CACertPath = "ca/rein-ca.pem" }, "must be absolute"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := goldenParams()
			c.mut(&p)
			_, err := Build(p)
			if err == nil {
				t.Fatalf("Build succeeded; want error containing %q", c.wantS)
			}
			if !strings.Contains(err.Error(), c.wantS) {
				t.Errorf("error = %q, want substring %q", err, c.wantS)
			}
		})
	}
}

// TestValidate_RejectsTampered proves Validate (the post-build fail-closed gate)
// catches a profile a caller mutated into an insecure state.
func TestValidate_RejectsTampered(t *testing.T) {
	mut := []struct {
		name string
		mut  func(*Profile)
	}{
		{"drop af_unix_mediation", func(pr *Profile) { pr.Linux.AfUnixMediation = "" }},
		{"drop deny_credentials", func(pr *Profile) { pr.Groups.Include = []string{"deny_shell_history"} }},
		{"inject host bypassed", func(pr *Profile) {
			pr.Network.UpstreamBypass = append(pr.Network.UpstreamBypass, proxy.InjectHosts[0])
		}},
		{"wildcard shadows inject host in bypass", func(pr *Profile) {
			pr.Network.UpstreamBypass = append(pr.Network.UpstreamBypass, "*.github.com")
		}},
		{"extra host in bypass", func(pr *Profile) {
			pr.Network.UpstreamBypass = append(pr.Network.UpstreamBypass, "api.anthropic.com")
		}},
		{"upstream_proxy repointed to url", func(pr *Profile) {
			pr.Network.UpstreamProxy = "http://attacker:1234"
		}},
		{"CDN not bypassed", func(pr *Profile) { pr.Network.UpstreamBypass = nil }},
		{"block true", func(pr *Profile) { pr.Network.Block = true }},
		{"no CA env", func(pr *Profile) { delete(pr.Environment.SetVars, caEnvVars[0]) }},
		{"no CA read grant", func(pr *Profile) { pr.Filesystem.ReadFile = nil }},
	}
	for _, m := range mut {
		t.Run(m.name, func(t *testing.T) {
			pr := mustBuild(t, goldenParams())
			m.mut(&pr)
			if err := pr.Validate(); err == nil {
				t.Errorf("Validate accepted a tampered profile (%s)", m.name)
			}
		})
	}
}

// TestNonoValidate runs the real nono binary against the generated profile.
// Skipped when nono is absent so CI without nono still passes.
func TestNonoValidate(t *testing.T) {
	bin := nonoBin()
	if bin == "" {
		t.Skip("nono binary not found; skipping real-binary validation")
	}
	pr := mustBuild(t, goldenParams())
	b, err := pr.MarshalIndent()
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(f, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "profile", "validate", f)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nono profile validate failed: %v\n%s", err, out)
	}
	t.Logf("nono profile validate: %s", out)
}

func nonoBin() string {
	if p, err := exec.LookPath("nono"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	cand := filepath.Join(home, ".local", "bin", "nono")
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	return ""
}
