package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/srt"
)

// The fixture's coverage of the REAL policy is enforced at run time: the
// policy lists live in package main of cmd/rein, out of a test's reach.
func TestRequirePlanted(t *testing.T) {
	home := t.TempDir()
	if err := writeFixture(home); err != nil {
		t.Fatal(err)
	}
	planted := []string{filepath.Join(home, ".ssh"), filepath.Join(home, ".netrc")}
	if err := requirePlanted(home, planted); err != nil {
		t.Errorf("planted entries rejected: %v", err)
	}
	if err := os.Mkdir(filepath.Join(home, ".newcreds"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".emptyfile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, entries := range map[string][]string{
		"entry absent from the fixture": append(planted, filepath.Join(home, ".absent")),
		"entry is an empty dir":         append(planted, filepath.Join(home, ".newcreds")),
		"entry is a zero-byte file":     append(planted, filepath.Join(home, ".emptyfile")),
		"no entry under home at all":    {"/etc/shadow"},
	} {
		if err := requirePlanted(home, entries); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBuildTargets(t *testing.T) {
	home := t.TempDir()
	if err := writeFixture(home); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	withContent := filepath.Join(outside, "gh")
	if err := os.MkdirAll(withContent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(withContent, "hosts.yml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(outside, "git")
	// Only a zero-byte file: unreadable in-sandbox would prove nothing.
	hollow := filepath.Join(outside, "hollow")
	if err := os.MkdirAll(hollow, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hollow, "lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(home, "overlay")
	cfg := srt.Config{
		Network: srt.Network{AllowedDomains: []string{reachableControl}},
		Filesystem: srt.Filesystem{
			DenyRead:   []string{home, filepath.Join(home, ".ssh"), withContent, absent, hollow},
			AllowRead:  []string{filepath.Join(home, "go"), filepath.Join(home, ".nvm")},
			AllowWrite: []string{overlay},
		},
	}
	// .nvm is an allow-back the fixture does not plant.
	if _, _, err := buildTargets(cfg, home); err == nil || !strings.Contains(err.Error(), ".nvm") {
		t.Fatalf("unplanted allow-back accepted: %v", err)
	}
	cfg.Filesystem.AllowRead = cfg.Filesystem.AllowRead[:1]
	tg, nothingToHide, err := buildTargets(cfg, home)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(nothingToHide, ",") != absent+","+hollow {
		t.Errorf("nothingToHide = %v, want %s and %s", nothingToHide, absent, hollow)
	}
	if strings.Join(tg.Network, ",") != reachableControl+","+deniedCanary {
		t.Errorf("network targets = %v", tg.Network)
	}
	except := map[string]string{}
	for _, f := range tg.Files {
		except[f.Target] = strings.Join(f.Except, ",")
	}
	// The home deny is judged outside the binds nested in it, and nothing else is.
	want := strings.Join([]string{filepath.Join(home, "go"), overlay}, ",")
	if got := except[home]; got != want {
		t.Errorf("except for home = %q, want %q", got, want)
	}
	for _, target := range []string{filepath.Join(home, ".ssh"), filepath.Join(home, "go"), withContent} {
		if e, ok := except[target]; !ok || e != "" {
			t.Errorf("target %s: present=%v except=%q", target, ok, e)
		}
	}
}

func TestWriteObservations(t *testing.T) {
	dir := t.TempDir()
	persisted, gone := filepath.Join(dir, "persisted"), filepath.Join(dir, "gone")
	if err := os.WriteFile(persisted, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	obs := writeObservations([]string{persisted, gone})
	if !obs[0].Reachable || obs[1].Reachable {
		t.Errorf("got %+v", obs)
	}
	if _, err := os.Stat(persisted); err == nil {
		t.Error("persisted probe file was not removed")
	}
}
