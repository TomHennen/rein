package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/srt"
)

func TestFixtureCoversPolicy(t *testing.T) {
	home := t.TempDir()
	if err := writeFixture(home); err != nil {
		t.Fatal(err)
	}
	for _, rel := range append(append([]string{".gitconfig"}, fixtureFiles...), HomeSentinels...) {
		if st, err := os.Stat(filepath.Join(home, rel)); err != nil || st.Size() == 0 {
			t.Errorf("fixture file %s missing or empty", rel)
		}
	}

	planted := []string{filepath.Join(home, ".ssh"), filepath.Join(home, ".netrc")}
	if err := requirePlanted(home, planted); err != nil {
		t.Errorf("planted entries rejected: %v", err)
	}
	if err := os.Mkdir(filepath.Join(home, ".newcreds"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, entries := range map[string][]string{
		"entry absent from the fixture": append(planted, filepath.Join(home, ".absent")),
		"entry is an empty dir":         append(planted, filepath.Join(home, ".newcreds")),
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
	cfg := srt.Config{
		Network: srt.Network{AllowedDomains: []string{reachableControl}},
		Filesystem: srt.Filesystem{
			DenyRead:  []string{home, filepath.Join(home, ".ssh"), withContent, absent},
			AllowRead: []string{filepath.Join(home, "go"), filepath.Join(home, ".nvm")},
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
	if len(nothingToHide) != 1 || nothingToHide[0] != absent {
		t.Errorf("nothingToHide = %v, want only %s", nothingToHide, absent)
	}
	if strings.Join(tg.Network, ",") != reachableControl+","+deniedCanary {
		t.Errorf("network targets = %v", tg.Network)
	}
	probes := map[string]string{}
	for _, f := range tg.Files {
		probes[f.Target] = f.Probe
	}
	// A denied dir is read through a real file inside it, reported under the entry.
	if got := probes[withContent]; got != filepath.Join(withContent, "hosts.yml") {
		t.Errorf("probe for %s = %q", withContent, got)
	}
	for _, want := range []string{filepath.Join(home, ".ssh"), filepath.Join(home, "go"), filepath.Join(home, HomeSentinels[0])} {
		if _, ok := probes[want]; !ok {
			t.Errorf("no file target for %s", want)
		}
	}
}
