package containment

import (
	"path/filepath"
	"testing"

	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/srt"
)

// buildConfig produces a realistic emitted config via srt.Build, so the oracle
// is pinned to the SHAPE rein actually writes (not a hand-rolled Config).
func buildConfig(t *testing.T) srt.Config {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "repo")
	cred := filepath.Join(root, "home", ".config", "gh")
	appPem := filepath.Join(root, "home", ".config", "rein-credentials")
	sock := filepath.Join(root, "run", "rein.sock")
	cfg, err := srt.Build(srt.Params{
		SocketPath:          sock,
		WorkingTree:         work,
		ExtraAllowedDomains: []string{"api.anthropic.com"},
		DenyReadCredStores:  []string{cred, appPem},
		RuntimeDenyRead:     []string{filepath.Join(root, "run", "user")},
	})
	if err != nil {
		t.Fatalf("srt.Build: %v", err)
	}
	return cfg
}

func TestOracle_Network(t *testing.T) {
	o := NewOracle(buildConfig(t))
	cases := []struct {
		name string
		obs  Observation
		want Verdict
	}{
		{"inject host reachable + token", Observation{KindNetwork, "api.github.com", true, true}, VerdictOK},
		{"inject host reachable no token", Observation{KindNetwork, "api.github.com", true, false}, VerdictRegression},
		{"git host token injected", Observation{KindNetwork, "github.com", true, true}, VerdictOK},
		{"CDN host reachable un-injected", Observation{KindNetwork, "codeload.github.com", true, false}, VerdictOK},
		{"CDN host with token is a LEAK", Observation{KindNetwork, "codeload.github.com", true, true}, VerdictLeak},
		{"extra egress reachable", Observation{KindNetwork, "api.anthropic.com", true, false}, VerdictOK},
		{"extra egress with token is a LEAK", Observation{KindNetwork, "api.anthropic.com", true, true}, VerdictLeak},
		{"denied host reachable is a LEAK", Observation{KindNetwork, "evil.example.com", true, false}, VerdictLeak},
		{"denied host blocked is OK", Observation{KindNetwork, "evil.example.com", false, false}, VerdictOK},
		{"allowed host unreachable is a regression", Observation{KindNetwork, "api.github.com", false, false}, VerdictRegression},
		{"case/dot normalized", Observation{KindNetwork, "API.GitHub.com.", true, true}, VerdictOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := o.Classify(c.obs).Verdict; got != c.want {
				t.Errorf("%s: got %s, want %s", c.name, got, c.want)
			}
		})
	}
}

func TestOracle_File(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "repo")
	cred := filepath.Join(root, "home", ".config", "gh")
	cfg, err := srt.Build(srt.Params{
		SocketPath:         filepath.Join(root, "run", "rein.sock"),
		WorkingTree:        work,
		DenyReadCredStores: []string{cred},
		RuntimeDenyRead:    []string{filepath.Join(root, "run", "user")},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	o := NewOracle(cfg)

	// A file under a denyRead cred store, still readable in-sandbox → leak.
	leak := Observation{KindFile, filepath.Join(cred, "hosts.yml"), true, false}
	if got := o.Classify(leak); got.Verdict != VerdictLeak {
		t.Errorf("readable cred file: got %s, want leak (%s)", got.Verdict, got.Reason)
	}
	// Same file correctly hidden → ok.
	hidden := Observation{KindFile, filepath.Join(cred, "hosts.yml"), false, false}
	if got := o.Classify(hidden); got.Verdict != VerdictOK {
		t.Errorf("hidden cred file: got %s, want ok", got.Verdict)
	}
	// The exact denyRead dir itself, readable → leak.
	if got := o.Classify(Observation{KindFile, cred, true, false}); got.Verdict != VerdictLeak {
		t.Errorf("readable denyRead dir: got %s, want leak", got.Verdict)
	}
	// A path outside any denyRead → unknown (triage, never silently ok).
	if got := o.Classify(Observation{KindFile, filepath.Join(work, "README"), true, false}); got.Verdict != VerdictUnknown {
		t.Errorf("uncovered path: got %s, want unknown", got.Verdict)
	}
}

// TestOracle_AllowReadReexposed covers the #59 home-deny model: the working
// tree + a curated toolchain are re-exposed read-only via allowRead UNDER a
// wholesale $HOME denyRead. A path under the deeper allowRead is expected-
// readable, not a leak; a cred store (deeper denyRead) under the same home deny
// is still a leak.
func TestOracle_AllowReadReexposed(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cred := filepath.Join(home, ".config", "gh")
	tool := filepath.Join(home, ".toolchain")
	cfg, err := srt.Build(srt.Params{
		SocketPath:         filepath.Join(root, "run", "rein.sock"),
		WorkingTree:        filepath.Join(home, "repo"),
		DenyReadHome:       home,
		DenyReadCredStores: []string{cred},
		RuntimeDenyRead:    []string{filepath.Join(root, "run", "user")},
		AllowRead:          []string{tool},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	o := NewOracle(cfg)

	// Under the allowRead re-bind, readable → expected (ok).
	if got := o.Classify(Observation{KindFile, filepath.Join(tool, "bin", "node"), true, false}); got.Verdict != VerdictOK {
		t.Errorf("allowRead re-exposed path: got %s, want ok (%s)", got.Verdict, got.Reason)
	}
	// The allowRead path unreadable → regression (agent needs it), not a leak.
	if got := o.Classify(Observation{KindFile, filepath.Join(tool, "bin", "node"), false, false}); got.Verdict != VerdictRegression {
		t.Errorf("allowRead path unreadable: got %s, want regression", got.Verdict)
	}
	// Cred store under the SAME home deny but deeper-denied → still a leak.
	if got := o.Classify(Observation{KindFile, filepath.Join(cred, "hosts.yml"), true, false}); got.Verdict != VerdictLeak {
		t.Errorf("cred under home deny readable: got %s, want leak", got.Verdict)
	}
	// A plain path under the home deny with no allowRead → leak.
	if got := o.Classify(Observation{KindFile, filepath.Join(home, ".secret"), true, false}); got.Verdict != VerdictLeak {
		t.Errorf("plain home path readable: got %s, want leak", got.Verdict)
	}
}

func TestOracle_Env(t *testing.T) {
	o := NewOracle(buildConfig(t))
	if got := o.Classify(Observation{KindEnv, "ANTHROPIC_API_KEY", true, false}); got.Verdict != VerdictLeak {
		t.Errorf("present sensitive env: got %s, want leak", got.Verdict)
	}
	if got := o.Classify(Observation{KindEnv, "anthropic_api_key", true, false}); got.Verdict != VerdictLeak {
		t.Errorf("sensitive env is case-insensitive: got %s, want leak", got.Verdict)
	}
	if got := o.Classify(Observation{KindEnv, "GH_TOKEN", false, false}); got.Verdict != VerdictOK {
		t.Errorf("scrubbed sensitive env: got %s, want ok", got.Verdict)
	}
	if got := o.Classify(Observation{KindEnv, "PATH", true, false}); got.Verdict != VerdictUnknown {
		t.Errorf("non-sensitive env: got %s, want unknown", got.Verdict)
	}
}

func TestOracle_HasLeakAndSort(t *testing.T) {
	o := NewOracle(buildConfig(t))
	obs := []Observation{
		{KindNetwork, "api.github.com", true, true},    // ok
		{KindNetwork, "evil.example.com", true, false}, // leak
		{KindEnv, "PATH", true, false},                 // unknown
	}
	results := o.ClassifyAll(obs)
	if !HasLeak(results) {
		t.Fatal("HasLeak must be true when a leak is present")
	}
	if results[0].Verdict != VerdictLeak {
		t.Errorf("leaks must sort first, got %s", results[0].Verdict)
	}
}

// Guard that the oracle's expected-open set actually contains the managed hosts
// it must never flag — a smoke test that Build wired the config as assumed.
func TestOracle_ManagedHostsAreExpectedOpen(t *testing.T) {
	o := NewOracle(buildConfig(t))
	for _, h := range append(append([]string{}, proxy.InjectHosts...), proxy.CDNHosts...) {
		if !o.allowedDomains[normHost(h)] {
			t.Errorf("managed host %q missing from allowedDomains oracle set", h)
		}
	}
	for _, h := range proxy.InjectHosts {
		if !o.injectDomains[normHost(h)] {
			t.Errorf("inject host %q missing from injectDomains oracle set", h)
		}
	}
}
